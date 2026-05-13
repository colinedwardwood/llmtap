// Package proxy is the HTTP-level glue: it accepts incoming LLM API calls,
// matches them to an upstream from config, parses request and response with
// the matching provider, enriches the active span, records GenAI metrics,
// and forwards bytes unchanged to the caller.
//
// Streaming responses are tee-parsed in place — no goroutine per request,
// no buffering of the full body.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/colinedwardwood/llmtap/internal/auth"
	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/genai"
	"github.com/colinedwardwood/llmtap/internal/labels"
	"github.com/colinedwardwood/llmtap/internal/pricing"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/redact"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// maxEnrichmentBodyBytes caps how much of an inbound request body llmtap
// buffers into memory for parser-driven enrichment. Real LLM requests
// rarely exceed ~512 KiB; the few that do (vision, audio, agent traces)
// are forwarded byte-for-byte but the request span only gets the
// metadata that the head bytes happen to surface. The proxy is allowed
// to give up on enrichment; it is not allowed to give up on the bytes.
const maxEnrichmentBodyBytes = 1024 * 1024 // 1 MiB

// ctxKey scopes per-request data carried into ModifyResponse. We never share
// a *httputil.ReverseProxy field across goroutines for per-request state.
type ctxKey int

const (
	ctxKeyModify ctxKey = iota
)

type modifyFn func(*http.Response) error

// Handler is the proxy's net/http.Handler. Construct it with [New] and pass
// the result through [otelhttp.NewHandler] for self-instrumentation.
type Handler struct {
	cfg       config.Config
	providers provider.Registry
	rps       map[string]*httputil.ReverseProxy // by upstream name
	tracer    trace.Tracer
	meters    telemetry.GenAIMeters
	inflight  metric.Int64UpDownCounter
	requests  metric.Int64Counter
	logger    *slog.Logger

	// activeStreams is incremented on stream wrap and decremented on close.
	// Exposed via expvar-style debug if ever needed; the field exists so
	// shutdown can wait for streams to drain instead of guessing.
	activeStreams atomic.Int64

	// modelLabel bounds the cardinality of gen_ai.{request,response}.model
	// values on the metric path. Span attributes keep the raw model
	// string — span cardinality is governed by retention.
	modelLabel *labels.ModelLabel

	// pricing is the active cost catalogue (built-in defaults, optionally
	// overlaid with an operator file). Held per-Handler so
	// `cfg.Pricing.Path` actually changes the recorded cost.
	pricing *pricing.Table

	// auth gates inbound requests against an operator-configured
	// allow-list of bearer-token hashes. Nil = no auth required.
	auth       *auth.Verifier
	authHeader string

	// upstreamSem caps concurrent in-flight requests per upstream.
	// A nil chan means "unlimited" (no entry for that upstream means
	// it wasn't configured with max_in_flight). Buffered chans are
	// the simplest counting-semaphore primitive without pulling in
	// x/sync.
	upstreamSem map[string]chan struct{}

	// breakers holds the per-upstream circuit breaker. A missing entry
	// means the breaker is disabled for that upstream. State updates
	// happen from the responseInterceptor once the upstream status code
	// is known; admission checks happen in ServeHTTP.
	breakers map[string]*breaker

	// ready is the readiness probe used by /readyz. Set by callers via
	// telemetry.Providers.Ready; nil means "always ready" (no telemetry
	// gating). The function is invoked on every /readyz request, so it
	// must be cheap and lock-free.
	ready func() bool
}

// New builds a Handler from validated config and a Providers bundle. It
// returns an error if any upstream URL fails to parse or its provider is
// missing from the registry.
func New(cfg config.Config, providers provider.Registry, prov telemetry.Providers, logger *slog.Logger) (*Handler, error) {
	rps := make(map[string]*httputil.ReverseProxy, len(cfg.Upstreams))
	upstreamSem := make(map[string]chan struct{}, len(cfg.Upstreams))
	breakers := make(map[string]*breaker, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		if u.MaxInFlight > 0 {
			upstreamSem[u.Name] = make(chan struct{}, u.MaxInFlight)
		}
		if u.Breaker.Failures > 0 {
			breakers[u.Name] = newBreaker(u.Breaker)
		}
		if _, ok := providers[u.Provider]; !ok {
			return nil, fmt.Errorf("provider %q not registered (upstream %q)", u.Provider, u.Name)
		}
		target, err := url.Parse(u.Target)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: parse target: %w", u.Name, err)
		}
		transport, err := buildUpstreamTransport(u, target)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: build transport: %w", u.Name, err)
		}
		rp := &httputil.ReverseProxy{
			Transport: transport,
			// FlushInterval = -1 means flush on every write. Without
			// this, httputil buffers non-chunked responses (those
			// with a Content-Length) and the client's first byte
			// arrives only once the entire upstream body has been
			// drained — defeating the stream-through behaviour A28
			// is meant to provide. Streaming responses already opt
			// in to per-write flushing via the event-stream
			// content type; setting this here unifies the two
			// paths so non-streaming JSON also streams through.
			FlushInterval: -1,
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(target)
				r.Out.Host = target.Host
				// Hop-by-hop headers are stripped by httputil; we only
				// add a marker so the upstream can identify llmtap in
				// logs without ambiguity.
				r.Out.Header.Set("Via", "llmtap")
				// Strip the client's Accept-Encoding so http.Transport
				// injects its own and transparently decompresses any
				// gzip response. Without this, gzip-encoded JSON
				// reaches modify() unparsed and every token / cost
				// metric records as zero (A6).
				r.Out.Header.Del("Accept-Encoding")
			},
			ModifyResponse: func(resp *http.Response) error {
				if fn, ok := resp.Request.Context().Value(ctxKeyModify).(modifyFn); ok && fn != nil {
					return fn(resp)
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				logger.WarnContext(r.Context(), "upstream error",
					slog.String("upstream", target.String()),
					slog.Any("err", err),
				)
				w.WriteHeader(http.StatusBadGateway)
			},
		}
		rps[u.Name] = rp
	}

	infl, err := prov.Meter.Int64UpDownCounter(
		"llmtap.requests.in_flight",
		metric.WithDescription("Requests currently being proxied through llmtap."),
	)
	if err != nil {
		return nil, fmt.Errorf("inflight counter: %w", err)
	}
	reqs, err := prov.Meter.Int64Counter(
		"llmtap.requests.total",
		metric.WithDescription("Total requests handled by llmtap, labelled by upstream and outcome."),
	)
	if err != nil {
		return nil, fmt.Errorf("requests counter: %w", err)
	}

	priceTable, err := pricing.Load(cfg.Pricing.Path, cfg.Pricing.FailOpen)
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}

	verifier, err := auth.NewVerifier(cfg.Auth.Tokens)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}

	return &Handler{
		cfg:         cfg,
		providers:   providers,
		rps:         rps,
		tracer:      prov.Tracer,
		meters:      prov.Meters,
		inflight:    infl,
		requests:    reqs,
		logger:      logger,
		modelLabel:  labels.NewModelLabel(labels.DefaultMaxCardinality),
		pricing:     priceTable,
		auth:        verifier,
		authHeader:  cfg.Auth.HeaderName(),
		upstreamSem: upstreamSem,
		breakers:    breakers,
		ready:       prov.Ready,
	}, nil
}

// buildUpstreamTransport returns a per-upstream *http.Transport tuned for
// the proxy's workload. The defaults are tighter than http.DefaultTransport
// in the directions that matter for proxying LLM APIs:
//
//   - MaxIdleConnsPerHost = 64 lifts the stdlib's stingy 2-per-host cap
//     which serialises bursts and creates the appearance of a
//     concurrency bug under realistic load (A27).
//   - ResponseHeaderTimeout = 60s protects against upstream half-opens
//     where bytes never arrive after dial.
//   - ForceAttemptHTTP2 keeps multiplexed connections to providers that
//     speak HTTP/2.
//
// Per-upstream pinning (A18) is layered on top of the same Transport so
// pinning never breaks the connection pool tuning.
func buildUpstreamTransport(u config.Upstream, target *url.URL) (*http.Transport, error) {
	t := &http.Transport{
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// ServerName is normally inferred from the request URL, but
		// the proxy rewrites the request to target the upstream's
		// host, so we set it explicitly to make SNI deterministic.
		ServerName: target.Hostname(),
	}
	if len(u.PinSHA256) > 0 {
		pins, err := parsePins(u.PinSHA256)
		if err != nil {
			return nil, err
		}
		tlsCfg.VerifyPeerCertificate = makePinVerifier(pins)
		// VerifyConnection runs on EVERY handshake, including
		// resumed sessions, where VerifyPeerCertificate is skipped.
		// Without this, an attacker could fast-resume a session
		// from an unpinned upstream and bypass the pin check
		// entirely. We require the connection's peer chain leaf
		// SPKI to match the pin set on every connection.
		tlsCfg.VerifyConnection = makeConnVerifier(pins)
	}
	t.TLSClientConfig = tlsCfg
	return t, nil
}

// parsePins decodes operator-supplied hex strings into raw 32-byte
// SPKI digests. Validation also happens in Config.Validate; this is
// the second line of defence in case a caller bypasses Validate.
func parsePins(hexPins []string) ([][]byte, error) {
	out := make([][]byte, 0, len(hexPins))
	for _, p := range hexPins {
		b, err := hex.DecodeString(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pin %q: %w", p, err)
		}
		if len(b) != sha256.Size {
			return nil, fmt.Errorf("pin %q: want 32 bytes (64 hex chars), got %d", p, len(b))
		}
		out = append(out, b)
	}
	return out, nil
}

// makePinVerifier returns a VerifyPeerCertificate callback that
// accepts the chain only if the leaf certificate's SPKI sha256 matches
// one of the configured pins. This complements (does not replace)
// stdlib chain verification — the stdlib still validates the chain
// against the system trust store first.
func makePinVerifier(pins [][]byte) func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("upstream pin: peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("upstream pin: parse leaf: %w", err)
		}
		return matchPin(leaf, pins)
	}
}

// makeConnVerifier returns a VerifyConnection callback that runs on
// every handshake (including resumed sessions, where
// VerifyPeerCertificate is skipped) and rejects the connection if
// the peer's leaf SPKI doesn't match a pin.
func makeConnVerifier(pins [][]byte) func(cs tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("upstream pin: connection has no peer certificate")
		}
		return matchPin(cs.PeerCertificates[0], pins)
	}
}

// matchPin checks the leaf's SPKI sha256 against the pin set.
func matchPin(leaf *x509.Certificate, pins [][]byte) error {
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	for _, p := range pins {
		if bytesEqual(p, sum[:]) {
			return nil
		}
	}
	return fmt.Errorf("upstream pin: leaf SPKI sha256 %x not in pin set", sum[:])
}

// bytesEqual is a tiny constant-time-ish equality check on equal-length
// byte slices. We don't pull in crypto/subtle because the input length
// is fixed at 32 and operator pin lists are short.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health probes are answered before any auth gate and before
	// config.Match — operators (and Kubernetes) need to probe llmtap
	// without minting a bearer token, and the probes deliberately
	// short-circuit the upstream entirely.
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/healthz":
			h.serveHealthz(w)
			return
		case "/readyz":
			h.serveReadyz(w)
			return
		}
	}

	if h.auth.Enabled() && !h.checkAuth(r) {
		// 401 with no upstream contact. Do not echo the supplied token,
		// even truncated — a noisy 401 helps an attacker time their
		// guess.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	upstream, ok := h.cfg.Match(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rp, ok := h.rps[upstream.Name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Circuit-breaker admission check. Runs BEFORE the concurrency
	// semaphore so an open breaker doesn't burn a slot. When the
	// breaker is open / half-open with a probe in flight, the proxy
	// responds 503 + Retry-After without touching the upstream.
	if br := h.breakers[upstream.Name]; br != nil {
		if ok, retryAfter := br.admit(); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			http.Error(w, "upstream circuit breaker open", http.StatusServiceUnavailable)
			return
		}
	}

	// Per-upstream concurrency cap. Acquire BEFORE any body read or
	// span/metric work so a flood doesn't waste those resources.
	if sem := h.upstreamSem[upstream.Name]; sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "upstream concurrency limit reached", http.StatusTooManyRequests)
			return
		}
	}

	prv := h.providers[upstream.Provider]
	op := prv.OperationFor(r.URL.Path)

	if op == "" {
		// Path is owned by this upstream but isn't a recognised LLM
		// endpoint (e.g. /v1/files): forward without enrichment.
		rp.ServeHTTP(w, r)
		return
	}

	h.inflight.Add(r.Context(), 1, metric.WithAttributes(attribute.String("upstream", upstream.Name)))
	defer h.inflight.Add(r.Context(), -1, metric.WithAttributes(attribute.String("upstream", upstream.Name)))

	// Hard-cap pre-check: if the caller advertises a body above the
	// configured ceiling, refuse before we drain any bytes.
	if hardCap := h.cfg.Request.MaxBodyBytes; hardCap > 0 && r.ContentLength > hardCap {
		http.Error(w, "request body exceeds maximum", http.StatusRequestEntityTooLarge)
		return
	}

	body, err := captureHeadAndForward(r, h.cfg.Request.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "request body exceeds maximum", http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.WarnContext(r.Context(), "request body read failed; falling back to transparent proxy",
			slog.Any("err", err),
		)
		rp.ServeHTTP(w, r)
		return
	}

	ctx, span := h.tracer.Start(r.Context(),
		genai.SpanName(op, ""),
		trace.WithSpanKind(trace.SpanKindClient),
	)

	contentOpts := provider.ContentOpts{
		Capture: h.cfg.Content.Mode == config.CaptureEvents,
		Redact:  redact.Func(redact.Profile(h.cfg.Content.Redact)),
	}
	info := prv.ParseRequest(span, r.URL.Path, body, contentOpts)

	finalize, modify := h.responseInterceptor(prv, &info, contentOpts, span, upstream.Name)
	ctx = context.WithValue(ctx, ctxKeyModify, modify)
	rp.ServeHTTP(w, r.WithContext(ctx))

	// finalize covers the non-streaming path and any 4xx/5xx where the
	// streaming wrapper never closed. It is idempotent.
	finalize(ctx)
}

// responseInterceptor returns:
//   - finalize: must be called once after ServeHTTP returns. It records
//     metrics and ends the span if the streaming wrapper hasn't already.
//   - modify: the ReverseProxy.ModifyResponse hook. Typed as modifyFn so
//     the context-value type assertion in the Rewrite hook succeeds —
//     named function types and their unnamed underlying type are
//     distinct under runtime type assertions.
func (h *Handler) responseInterceptor(
	prv provider.Provider,
	info *provider.Info,
	content provider.ContentOpts,
	span trace.Span,
	upstreamName string,
) (finalize func(context.Context), modify modifyFn) {
	var (
		finalized  atomic.Bool
		statusCode int
		// errCapture, when non-nil, indicates the response was an error
		// (4xx/5xx). It tees a snippet from the body without buffering
		// the whole thing so the original stream forwards intact.
		errCapture *snippetCapture
	)

	finishSpan := func() {
		if statusCode >= 400 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", statusCode))
		} else if statusCode > 0 {
			span.SetStatus(codes.Ok, "")
		}
		span.SetAttributes(attribute.Int("http.response.status_code", statusCode))
		if errCapture != nil {
			span.SetAttributes(attribute.Int("http.response.body_size", errCapture.total))
			if content.Capture {
				span.SetAttributes(attribute.String(
					"http.response.body_snippet",
					content.Clean(truncateUTF8(string(errCapture.head), 1024)),
				))
			}
		}
		// Fold the upstream's response into the circuit breaker so a
		// burst of 5xx trips it without needing per-request bookkeeping
		// at the call site. statusCode = 0 means the proxy never got a
		// response (e.g. ErrorHandler took over with 502); that's an
		// upstream-side fault — count it as a 502.
		if br := h.breakers[upstreamName]; br != nil {
			code := statusCode
			if code == 0 {
				code = http.StatusBadGateway
			}
			br.report(code)
		}
		span.End()
	}

	recordMetrics := func(ctx context.Context) {
		// Bound model-label cardinality before it hits any metric. Pricing
		// uses the raw model so snapshot suffixes still match the table.
		reqModelLabel := h.modelLabel.Normalize(info.RequestModel)
		respModelLabel := h.modelLabel.Normalize(info.ResponseModel)
		attrs := metric.WithAttributes(
			attribute.String(genai.AttrSystem, info.System),
			attribute.String(genai.AttrOperationName, info.Operation),
			attribute.String(genai.AttrRequestModel, reqModelLabel),
			attribute.String("upstream", upstreamName),
			attribute.Int("http.response.status_code", statusCode),
		)
		if d := info.DurationSeconds(); d > 0 {
			h.meters.OperationDuration.Record(ctx, d, attrs)
		}
		if t := info.TimeToFirstTokenSeconds(); t > 0 {
			h.meters.TimeToFirstToken.Record(ctx, t, attrs)
			span.SetAttributes(attribute.Float64(genai.AttrTimeToFirstToken, t))
		}
		if info.InputTokens > 0 {
			h.meters.TokenUsage.Record(ctx, info.InputTokens, metric.WithAttributes(
				attribute.String(genai.AttrSystem, info.System),
				attribute.String(genai.AttrOperationName, info.Operation),
				attribute.String(genai.AttrRequestModel, reqModelLabel),
				attribute.String("token_type", genai.TokenTypeInput),
			))
		}
		if info.OutputTokens > 0 {
			h.meters.TokenUsage.Record(ctx, info.OutputTokens, metric.WithAttributes(
				attribute.String(genai.AttrSystem, info.System),
				attribute.String(genai.AttrOperationName, info.Operation),
				attribute.String(genai.AttrRequestModel, reqModelLabel),
				attribute.String("token_type", genai.TokenTypeOutput),
			))
		}
		model := info.ResponseModel
		if model == "" {
			model = info.RequestModel
		}
		if usd, ok := h.pricing.Cost(info.System, model, info.InputTokens, info.OutputTokens); ok {
			costAttrs := metric.WithAttributes(
				attribute.String(genai.AttrSystem, info.System),
				attribute.String(genai.AttrRequestModel, reqModelLabel),
				attribute.String(genai.AttrResponseModel, respModelLabel),
			)
			// Per-call distribution + monotonic cumulative. The
			// histogram serves p50 / p95 cost dashboards; the
			// counter sidesteps float drift on the histogram _sum
			// across millions of small adds.
			h.meters.CostUSD.Record(ctx, usd, costAttrs)
			h.meters.CostUSDTotal.Add(ctx, usd, costAttrs)
			span.SetAttributes(attribute.Float64(genai.AttrCostUSD, usd))
		}
		h.requests.Add(ctx, 1, attrs)
	}

	finalize = func(ctx context.Context) {
		if !finalized.CompareAndSwap(false, true) {
			return
		}
		if info.Finished.IsZero() {
			info.Finished = time.Now()
		}
		recordMetrics(ctx)
		finishSpan()
	}

	modify = func(resp *http.Response) error {
		statusCode = resp.StatusCode
		info.FirstByteAt = time.Now()
		span.SetAttributes(attribute.String(genai.AttrSystem, info.System))

		if statusCode >= 400 {
			// Error bodies must reach the client byte-for-byte — they
			// frequently carry the structured detail the caller needs
			// to debug. We peel off a bounded snippet for the span via
			// an io.TeeReader; the rest of the bytes flow through
			// untouched. The snippet itself is gated on captureContent
			// since OpenAI/Anthropic error bodies routinely echo a
			// prefix of the offending API key, which would otherwise
			// leak into traces under the privacy-off default.
			//
			// Wrap the body in parseOnClose so finalize() fires on
			// EOF — before httputil's copy loop reports the body as
			// drained. Without this, FlushInterval=-1 streams the
			// error body to the client, the client's ReadAll
			// returns, and a test that probes rec.Ended()
			// immediately afterwards sees an empty set because
			// span.End() hasn't run yet on the server side.
			streamCtx := context.WithoutCancel(resp.Request.Context())
			errCapture = &snippetCapture{headCap: 1024}
			origCloser := resp.Body
			teed := io.TeeReader(resp.Body, errCapture)
			resp.Body = &parseOnClose{
				Reader:  teed,
				closer:  origCloser,
				onClose: func() { finalize(streamCtx) },
			}
			return nil
		}

		if isEventStream(resp.Header) {
			h.activeStreams.Add(1)
			info.Stream = true
			// Keep the request's trace context alive for the metric
			// emission that finalize will do when the stream closes.
			// `context.WithoutCancel` preserves baggage/span context
			// while detaching from the request's cancellation chain —
			// metrics for a completed stream should fire even if the
			// client has already disconnected.
			streamCtx := context.WithoutCancel(resp.Request.Context())
			resp.Body = prv.WrapStream(span, info, resp.Body, content,
				func() {}, // first-token bookkeeping is internal to provider
				func() {
					h.activeStreams.Add(-1)
					finalize(streamCtx)
				},
			)
			return nil
		}

		// Non-streaming JSON: stream-through-with-tee. The old
		// implementation drained the entire body into memory before
		// returning, which delayed the client's first byte by the
		// full upstream duration. Now the body forwards through a
		// TeeReader while a bounded snippetCapture accumulates up
		// to maxEnrichmentBodyBytes for the parser. The parser and
		// finalize() both fire as soon as the upstream reader hits
		// EOF — BEFORE the EOF propagates to httputil's copy loop.
		// That ordering matters: the server-side write of the
		// trailing bytes is still in httputil's hands, but the span
		// is finalised before httputil reports the upstream as
		// drained, so the very next thing the test (or operator
		// probe) sees on the span is "ended".
		streamCtx := context.WithoutCancel(resp.Request.Context())
		capture := &snippetCapture{headCap: maxEnrichmentBodyBytes}
		origCloser := resp.Body
		teed := io.TeeReader(resp.Body, capture)
		resp.Body = &parseOnClose{
			Reader: teed,
			closer: origCloser,
			onClose: func() {
				prv.ParseResponseJSON(span, info, capture.head, content)
				finalize(streamCtx)
			},
		}
		return nil
	}
	return finalize, modify
}

// parseOnClose wraps a body so the parser runs exactly once, on Close
// OR on the first Read that surfaces io.EOF (whichever comes first).
// Running on EOF (instead of waiting for Close) matters: the bytes
// httputil writes to the client are emitted as Read returns them;
// EOF is the signal that the upstream is drained AND the response
// is complete. Folding the parse into the EOF handler means span
// attributes are populated *before* the EOF propagates back through
// httputil's copy loop, which is the only deterministic moment
// before bytes hit the wire. The sync.Once guards against repeat
// invocations from any source: double-Close (io.NopCloser, httputil,
// client code), EOF followed by Close, or a buggy provider that
// re-reads the body.
type parseOnClose struct {
	io.Reader
	closer  io.Closer
	onClose func()
	once    sync.Once
}

func (p *parseOnClose) Read(b []byte) (int, error) {
	n, err := p.Reader.Read(b)
	if errors.Is(err, io.EOF) {
		p.once.Do(p.onClose)
	}
	return n, err
}

func (p *parseOnClose) Close() error {
	p.once.Do(p.onClose)
	return p.closer.Close()
}

// serveHealthz answers liveness probes. Always 200; the endpoint is
// proof that the process is running, nothing more.
func (h *Handler) serveHealthz(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// serveReadyz answers readiness probes. 200 once telemetry is ready;
// 503 + Retry-After: 1 otherwise. Loose semantics — a nil ready
// callback is treated as "always ready" because not every embedding
// of llmtap will plumb a real signal through.
func (h *Handler) serveReadyz(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	ready := h.ready
	if ready == nil || ready() {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ready")
		return
	}
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, "not ready")
}

// errBodyTooLarge signals that an inbound request body exceeds the
// configured hard cap. Callers translate this to an HTTP 413.
var errBodyTooLarge = errors.New("request body exceeds maximum")

// snippetCapture is an io.Writer used as the side of an io.TeeReader.
// It records the first headCap bytes of what's written (for span
// attribute attachment) and counts the total bytes seen (for size
// telemetry). Subsequent writes past headCap silently advance only the
// counter — the response body itself flows through the tee unchanged.
type snippetCapture struct {
	head    []byte
	headCap int
	total   int
}

func (s *snippetCapture) Write(p []byte) (int, error) {
	s.total += len(p)
	if remaining := s.headCap - len(s.head); remaining > 0 {
		take := len(p)
		if take > remaining {
			take = remaining
		}
		s.head = append(s.head, p[:take]...)
	}
	return len(p), nil
}

// captureHeadAndForward buffers up to maxEnrichmentBodyBytes of r.Body
// for parser-driven enrichment, then reconstitutes r.Body so the proxy
// can forward the original byte stream unchanged — even if it exceeds
// the enrichment buffer.
//
// hardCap (0 = unlimited) is enforced *while* draining the body. If the
// stream surpasses hardCap, errBodyTooLarge is returned and the caller
// is expected to respond 413 without forwarding.
func captureHeadAndForward(r *http.Request, hardCap int64) ([]byte, error) {
	// Read enough to (a) populate the enrichment slice and (b) detect
	// that the body is larger than the enrichment buffer.
	limited := io.LimitReader(r.Body, maxEnrichmentBodyBytes+1)
	head, err := io.ReadAll(limited)
	if err != nil {
		_ = r.Body.Close()
		return nil, err
	}

	if int64(len(head)) <= maxEnrichmentBodyBytes {
		// Whole body fits in the enrichment buffer. Close the original
		// reader and serve the buffered bytes to the upstream.
		_ = r.Body.Close()
		if hardCap > 0 && int64(len(head)) > hardCap {
			return nil, errBodyTooLarge
		}
		r.Body = io.NopCloser(bytes.NewReader(head))
		r.ContentLength = int64(len(head))
		return head, nil
	}

	// Body exceeds enrichment buffer. We've captured maxEnrichmentBodyBytes+1
	// bytes in `head`; the last byte is "lookahead" proving more data
	// exists. Reassemble r.Body as [head + remainder] so the forwarded
	// stream stays byte-for-byte identical to the inbound one. The
	// enrichment slice returned to the caller is truncated to the
	// enrichment cap; ParseRequest may fail to unmarshal incomplete
	// JSON, in which case the proxy emits a request span with the
	// metadata it managed to recover and zero else.
	closer := r.Body
	rest := &capCountingReader{src: r.Body, limit: hardCap, alreadyRead: int64(len(head))}
	r.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.MultiReader(bytes.NewReader(head), rest),
		Closer: closer,
	}
	r.ContentLength = -1 // chunked: total size unknown to us
	return head[:maxEnrichmentBodyBytes], nil
}

// capCountingReader wraps an io.Reader and aborts when the cumulative
// byte count would exceed limit (where the initial alreadyRead counts
// against limit). limit == 0 means no cap.
type capCountingReader struct {
	src         io.Reader
	limit       int64
	alreadyRead int64
}

func (c *capCountingReader) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	if n > 0 {
		c.alreadyRead += int64(n)
		if c.limit > 0 && c.alreadyRead > c.limit {
			return n, errBodyTooLarge
		}
	}
	return n, err
}

func isEventStream(h http.Header) bool {
	ct := h.Get("Content-Type")
	return strings.HasPrefix(ct, "text/event-stream")
}

// truncateUTF8 trims s to at most n bytes without splitting a multi-byte rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// checkAuth pulls the bearer token out of the configured request
// header, strips the optional "Bearer " prefix, and asks the verifier
// to constant-time match it. Also strips the header from the inbound
// request so it does not get forwarded to upstream (the verified
// token is llmtap's secret, not the LLM provider's).
func (h *Handler) checkAuth(r *http.Request) bool {
	raw := r.Header.Get(h.authHeader)
	r.Header.Del(h.authHeader)
	if raw == "" {
		return false
	}
	// Accept either `Bearer <token>` or a bare token, so operators
	// can choose between matching standard reverse-proxy convention
	// and using a custom header naked.
	raw = strings.TrimPrefix(raw, "Bearer ")
	return h.auth.Verify(raw)
}

// WrapWithOTel returns the handler wrapped in otelhttp middleware so llmtap's
// own HTTP server is itself traced. Caller passes the route name.
func (h *Handler) WrapWithOTel(routeName string) http.Handler {
	return otelhttp.NewHandler(h, routeName,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}

// WaitForStreams blocks until the in-flight stream counter reaches zero or
// ctx expires. It returns the remaining count at return time (0 = clean
// drain, >0 = ctx deadline reached with that many streams still open).
//
// http.Server.Shutdown waits for the response handler goroutines it spawns
// to return, but our SSE responses run their finalize from an onClose
// callback inside the response body wrapper — Shutdown sees the handler
// goroutine as "returned" the moment ServeHTTP exits, even though the
// stream is still actively writing bytes. Polling activeStreams closes
// that gap: shutdown only proceeds once the parser side of every stream
// has actually drained (or we hit the deadline).
//
// 50ms is a deliberate cadence: tight enough that a fast-finishing stream
// doesn't make Shutdown's deadline budget feel sluggish, loose enough not
// to consume measurable CPU during a long graceful-shutdown window.
func (h *Handler) WaitForStreams(ctx context.Context) int64 {
	if n := h.activeStreams.Load(); n == 0 {
		return 0
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return h.activeStreams.Load()
		case <-ticker.C:
			if n := h.activeStreams.Load(); n == 0 {
				return 0
			}
		}
	}
}
