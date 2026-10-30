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
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
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
	"github.com/colinedwardwood/llmtap/internal/replay"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
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

// statusClientClosedRequest is nginx's non-standard 499 "client closed
// request". Emitted when the client disconnects before the upstream
// answers, so metrics and the breaker can distinguish "caller went
// away" from an upstream-side 5xx fault.
const statusClientClosedRequest = 499

// ctxKey scopes per-request data carried into ModifyResponse. We never share
// a *httputil.ReverseProxy field across goroutines for per-request state.
type ctxKey int

const (
	ctxKeyModify ctxKey = iota
	ctxKeyCaller
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
	// requests counts EVERY outcome past the health probes — enriched,
	// passthrough, and reject — labelled by upstream + status. Used to
	// lie (status 0 / passthrough missing / rejects missing); fixed in D22.
	requests metric.Int64Counter
	// rejected counts operator / proxy rejects that never reached a
	// healthy upstream attempt (auth, breaker, concurrency, 413, 404).
	rejected metric.Int64Counter
	// authRejected counts bearer-token failures (401). Distinct from
	// auth_busy (429), which is overcrowding of the argon2 semaphore.
	authRejected metric.Int64Counter
	// breakerState is 0=closed / 1=open / 2=half_open per upstream.
	breakerState metric.Int64Gauge
	// streamsActive is the OTel twin of activeStreams; WaitForStreams
	// still reads the atomic, dashboards read the gauge counter.
	streamsActive metric.Int64UpDownCounter
	// streamIdleTimeouts counts post-header upstream body stalls that
	// exceeded http.stream_idle_timeout and were force-closed.
	streamIdleTimeouts metric.Int64Counter
	// certNotAfter is unix seconds of the currently-served leaf
	// certificate's NotAfter. Zero when TLS is off. Operators alert
	// when (certNotAfter - now) drops below a week.
	certNotAfter metric.Int64Gauge
	// certReloadFailures counts failed hot-reload attempts (torn
	// reads, parse errors). The previous cert stays active.
	certReloadFailures metric.Int64Counter
	// pricingMisses counts token-bearing responses the cost pipeline
	// could not price. Silent misses were the failure mode: the
	// pricing table ages, new models appear, and the spend dashboard
	// quietly undercounts exactly when spend grows.
	pricingMisses metric.Int64Counter
	logger        *slog.Logger
	contentLogger otellog.Logger

	// activeStreams is incremented on stream wrap and decremented on close.
	// Shutdown drains against this atomic; streamsActive mirrors it for
	// OTel export.
	activeStreams atomic.Int64

	// modelLabel bounds the cardinality of gen_ai.{request,response}.model
	// values on the metric path. Span attributes keep the raw model
	// string — span cardinality is governed by retention.
	modelLabel  *labels.ModelLabel
	callerLabel *labels.ModelLabel

	// pricing is the active cost catalogue (built-in defaults, optionally
	// overlaid with an operator file). Held per-Handler so
	// `cfg.Pricing.Path` actually changes the recorded cost.
	pricing  *pricing.Table
	recorder *replay.Recorder

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
	// happen from ServeHTTP's admission release once the upstream
	// status code is known; admission checks happen in ServeHTTP.
	breakers map[string]*breaker

	// ready is the readiness probe used by /readyz. Set by callers via
	// telemetry.Providers.Ready; nil means "always ready" (no telemetry
	// gating). The function is invoked on every /readyz request, so it
	// must be cheap and lock-free.
	ready func() bool
	// beginDrain flips /readyz to not-ready on SIGTERM. Nil is a no-op.
	beginDrain func()
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
				// SetURL JOINS target.Path with the inbound path — it
				// never strips anything. Routing-only mount prefixes
				// (like the default /anthropic) must be removed here
				// or the upstream receives /anthropic/v1/messages and
				// 404s every request.
				if u.StripPrefix {
					stripRoutePrefix(r, u.Prefix, target)
				}
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
				// Drop inbound W3C baggage: clients must not inject
				// key/value pairs that llmtap would forward upstream
				// (latent header-injection channel; D29 / FR-17).
				r.Out.Header.Del("Baggage")
				r.In.Header.Del("Baggage")
			},
			ModifyResponse: func(resp *http.Response) error {
				if fn, ok := resp.Request.Context().Value(ctxKeyModify).(modifyFn); ok && fn != nil {
					return fn(resp)
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				// Classify before blaming the upstream. A client that
				// hung up (context canceled) or a request body that
				// blew the hard cap mid-stream is NOT an upstream
				// fault — writing an unconditional 502 here fed both
				// into the circuit breaker as consecutive failures,
				// so a client-side deploy that cancelled a handful of
				// in-flight requests opened the circuit against a
				// healthy upstream: a traffic-correlated false outage.
				status := http.StatusBadGateway
				switch {
				case isBodyTooLargeErr(err):
					status = http.StatusRequestEntityTooLarge
				case errors.Is(err, context.Canceled) || r.Context().Err() != nil:
					status = statusClientClosedRequest
				}
				// The GenAI span (when the enriched path started one)
				// rides the request context; record the transport
				// error so upstream-down debugging has more than an
				// empty span with status_code=0.
				if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
					span.RecordError(err)
				}
				logger.WarnContext(r.Context(), "upstream error",
					slog.String("upstream", target.String()),
					slog.Int("status", status),
					slog.Any("err", err),
				)
				switch status {
				case http.StatusRequestEntityTooLarge:
					writeProxyJSONError(w, r, status, "request body exceeds maximum", "body_too_large")
				case statusClientClosedRequest:
					// Client already gone — headers only, no body.
					setCorrelationHeaders(w, r)
					w.WriteHeader(status)
				default:
					writeProxyJSONError(w, r, status, "upstream request failed", "bad_gateway")
				}
			},
		}
		rps[u.Name] = rp
	}

	infl, err := prov.Meter.Int64UpDownCounter(
		"llmtap.requests.in_flight",
		metric.WithDescription("Requests currently being proxied through llmtap (enriched path only)."),
	)
	if err != nil {
		return nil, fmt.Errorf("inflight counter: %w", err)
	}
	reqs, err := prov.Meter.Int64Counter(
		"llmtap.requests.total",
		metric.WithDescription("Total requests handled by llmtap past the health probes, including passthrough and rejects. Labels: upstream, http.response.status_code."),
	)
	if err != nil {
		return nil, fmt.Errorf("requests counter: %w", err)
	}
	rejected, err := prov.Meter.Int64Counter(
		"llmtap.requests.rejected.total",
		metric.WithDescription("Requests rejected by llmtap before a normal upstream response. Labels: reason (auth|auth_busy|not_found|breaker_open|concurrency|body_too_large), upstream."),
	)
	if err != nil {
		return nil, fmt.Errorf("rejected counter: %w", err)
	}
	authRej, err := prov.Meter.Int64Counter(
		"llmtap.auth.rejected.total",
		metric.WithDescription("Bearer-token auth failures (HTTP 401). Does not include auth_busy (429)."),
	)
	if err != nil {
		return nil, fmt.Errorf("auth rejected counter: %w", err)
	}
	bState, err := prov.Meter.Int64Gauge(
		"llmtap.breaker.state",
		metric.WithDescription("Per-upstream circuit breaker state: 0=closed, 1=open, 2=half_open."),
	)
	if err != nil {
		return nil, fmt.Errorf("breaker state gauge: %w", err)
	}
	streams, err := prov.Meter.Int64UpDownCounter(
		"llmtap.streams.active",
		metric.WithDescription("Active SSE response streams currently being tee-parsed."),
	)
	if err != nil {
		return nil, fmt.Errorf("streams active counter: %w", err)
	}
	streamIdle, err := prov.Meter.Int64Counter(
		"llmtap.streams.idle_timeouts.total",
		metric.WithDescription("Upstream response bodies closed because no bytes arrived within http.stream_idle_timeout after headers (or between chunks). Labels: upstream."),
	)
	if err != nil {
		return nil, fmt.Errorf("stream idle timeouts counter: %w", err)
	}
	certNotAfter, err := prov.Meter.Int64Gauge(
		"llmtap.tls.cert_not_after",
		metric.WithDescription("Unix seconds of the currently-served TLS leaf certificate NotAfter. Zero when TLS is disabled."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("cert not-after gauge: %w", err)
	}
	certReloadFail, err := prov.Meter.Int64Counter(
		"llmtap.tls.cert_reload_failures.total",
		metric.WithDescription("Failed TLS certificate hot-reload attempts. The previously loaded cert continues to be served."),
	)
	if err != nil {
		return nil, fmt.Errorf("cert reload failures counter: %w", err)
	}
	priceMisses, err := prov.Meter.Int64Counter(
		"llmtap.pricing.misses.total",
		metric.WithDescription("Requests whose token usage could not be priced because the model has no entry in the active pricing catalogue. A rising rate means the cost dashboard is undercounting."),
	)
	if err != nil {
		return nil, fmt.Errorf("pricing misses counter: %w", err)
	}

	priceTable, err := pricing.Load(cfg.Pricing.Path, cfg.Pricing.FailOpen)
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}

	verifier, err := auth.NewVerifier(cfg.Auth.Tokens)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}

	h := &Handler{
		cfg:                cfg,
		providers:          providers,
		rps:                rps,
		tracer:             prov.Tracer,
		meters:             prov.Meters,
		inflight:           infl,
		requests:           reqs,
		rejected:           rejected,
		authRejected:       authRej,
		breakerState:       bState,
		streamsActive:      streams,
		streamIdleTimeouts: streamIdle,
		certNotAfter:       certNotAfter,
		certReloadFailures: certReloadFail,
		pricingMisses:      priceMisses,
		logger:             logger,
		contentLogger:      prov.Logger,
		modelLabel:         labels.NewModelLabel(labels.DefaultMaxCardinality),
		callerLabel:        labels.NewModelLabel(cfg.Attribution.MaxCardinality),
		pricing:            priceTable,
		recorder:           replay.NewRecorder(cfg.Recording.Path, redact.Func(redact.Profile(cfg.Content.Redact))),
		auth:               verifier,
		authHeader:         cfg.Auth.HeaderName(),
		upstreamSem:        upstreamSem,
		breakers:           breakers,
		ready:              prov.Ready,
		beginDrain:         prov.BeginDrain,
	}
	// Seed breaker gauges at closed and wire trip/recover observability.
	for name, br := range h.breakers {
		upstream := name
		h.setBreakerState(upstream, breakerClosed)
		br.onTrip = func() {
			h.setBreakerState(upstream, breakerOpen)
			h.logger.Warn("circuit breaker tripped",
				slog.String("upstream", upstream),
				slog.String("state", "open"),
			)
		}
		br.onRecover = func() {
			h.setBreakerState(upstream, breakerClosed)
			h.logger.Info("circuit breaker recovered",
				slog.String("upstream", upstream),
				slog.String("state", "closed"),
			)
		}
		br.onHalfOpen = func() {
			h.setBreakerState(upstream, breakerHalfOpen)
			h.logger.Info("circuit breaker half-open (probe admitted)",
				slog.String("upstream", upstream),
				slog.String("state", "half_open"),
			)
		}
	}
	return h, nil
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
	}
	// ServerName is normally inferred from the request URL, but the
	// proxy rewrites the request to target the upstream's host, so we
	// set it explicitly to make SNI deterministic. The TLS spec
	// (RFC 6066 §3) disallows IP literals in the SNI extension; many
	// upstreams + some hostile middleboxes reject the handshake when
	// it shows up. Skip the ServerName assignment when target.Hostname()
	// is a parseable IP — TLS will fall back to its IP-based default.
	if host := target.Hostname(); net.ParseIP(host) == nil {
		tlsCfg.ServerName = host
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

// stripRoutePrefix rewrites the outbound URL so the routing prefix is
// removed and the remainder is re-joined onto the target's base path.
// Runs after SetURL, which has already joined target.Path with the full
// inbound path. r.In carries the untouched inbound request.
func stripRoutePrefix(r *httputil.ProxyRequest, prefix string, target *url.URL) {
	rest := strings.TrimPrefix(r.In.URL.Path, strings.TrimSuffix(prefix, "/"))
	if rest == "" || rest[0] != '/' {
		rest = "/" + rest
	}
	r.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + rest
	// Drop RawPath: the recomputed Path re-encodes canonically, and a
	// stale RawPath that disagrees with Path would silently win.
	r.Out.URL.RawPath = ""
}

// isBodyTooLargeErr reports whether err is the mid-stream hard-cap
// abort from capCountingReader. The transport wraps body-read errors
// on their way out; errors.Is covers wrap chains, the string check
// covers the transport paths that flatten with %v instead of %w.
func isBodyTooLargeErr(err error) bool {
	return errors.Is(err, errBodyTooLarge) ||
		strings.Contains(err.Error(), errBodyTooLarge.Error())
}

// canonicalPath normalizes an inbound URL path for routing: dot
// segments are resolved and empty paths become "/". Routing on the raw
// path let /v1/../anthropic/... match the /v1 upstream while the
// upstream's own normalization served something else — routing,
// enrichment, and forwarding must all agree on one path identity.
// Trailing slashes survive (path.Clean strips them, proxies must not).
func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	cp := path.Clean(p)
	if cp == "." {
		return "/"
	}
	if cp[0] != '/' {
		cp = "/" + cp
	}
	if strings.HasSuffix(p, "/") && cp != "/" {
		cp += "/"
	}
	return cp
}

// matchPin checks the leaf's SPKI sha256 against the pin set. Uses
// crypto/subtle.ConstantTimeCompare so a partial-match timing oracle
// can't leak which pin is closest to the peer's actual SPKI digest.
func matchPin(leaf *x509.Certificate, pins [][]byte) error {
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	for _, p := range pins {
		if subtle.ConstantTimeCompare(p, sum[:]) == 1 {
			return nil
		}
	}
	return fmt.Errorf("upstream pin: leaf SPKI sha256 %x not in pin set", sum[:])
}

// ServeHTTP is the proxy's request entry point. The flow is a flat
// pipeline of named gates, each of which either rejects with a
// terminal HTTP status OR falls through to the next:
//
//	health probes  → /healthz, /readyz
//	auth gate      → 401 / 429
//	upstream match → 404
//	admit          → 503 (breaker) / 429 (concurrency)
//	body cap       → 413
//	dispatch       → transparent passthrough OR enriched pipeline
//
// The admit + release pair (C1) is the load-bearing invariant: every
// path that reaches admit() must reach exactly one release() call
// with the resolved status code, regardless of which gate downstream
// terminates the request. release() forwards the resolved status code
// to the breaker so the half-open probe slot is always cleared.
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

	if caller := h.callerForRequest(r); caller != "" {
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyCaller, caller))
	}
	if !h.applyAuthGate(w, r) {
		return
	}

	// Canonicalize once, before routing, so routing, enrichment, and
	// forwarding all see the same path. RawPath is dropped along with
	// the rewrite: Path re-encodes canonically on the way out.
	r.URL.Path = canonicalPath(r.URL.Path)
	r.URL.RawPath = ""

	upstream, rp, ok := h.matchUpstream(w, r)
	if !ok {
		return
	}

	// Admit: claim the breaker probe slot (if half-open) and the
	// per-upstream concurrency slot. From here on, every exit path
	// MUST go through adm.release() so the breaker doesn't get
	// stuck in half-open with probeInFlight=true.
	adm, ok := h.admit(w, r, upstream)
	if !ok {
		return
	}

	// statusCapture wraps the response writer so adm.release() can
	// read the final status code that reached the client and fold it
	// into the breaker. Without this wrapper, transparent passthrough
	// paths (op=="" and the body-read-error fallback) would never
	// report anything to the breaker. The deferred request counter
	// makes llmtap.requests.total cover passthrough + enriched alike.
	capW := &statusCaptureWriter{ResponseWriter: w}
	defer func() {
		adm.release(capW)
		h.recordRequest(r.Context(), upstream.Name, capW.status)
	}()

	h.dispatch(capW, r, upstream, rp)
}

// applyAuthGate runs the auth verifier. Returns true when the request
// is admitted, false when a response was written. authBusy maps to
// 429 (the argon2 semaphore is saturated); authRejected maps to 401.
func (h *Handler) applyAuthGate(w http.ResponseWriter, r *http.Request) bool {
	if !h.auth.Enabled() {
		return true
	}
	switch h.checkAuth(r) {
	case authOK:
		return true
	case authBusy:
		// argon2 semaphore saturated: refuse with 429 + Retry-After
		// instead of queueing more 64 MiB allocations behind the
		// existing in-flight work. The auth gate is intentionally
		// the loudest backpressure surface — operators see auth-cost
		// shedding before the upstream concurrency cap fires.
		w.Header().Set("Retry-After", "1")
		writeProxyPlainError(w, r, http.StatusTooManyRequests, "auth verifier busy")
		h.recordRejected(r.Context(), rejectReasonAuthBusy, "")
		h.recordRequest(r.Context(), "", http.StatusTooManyRequests)
		return false
	default:
		// 401 with no upstream contact. Do not echo the supplied token,
		// even truncated — a noisy 401 helps an attacker time their
		// guess.
		writeProxyPlainError(w, r, http.StatusUnauthorized, "unauthorized")
		h.authRejected.Add(r.Context(), 1)
		h.recordRejected(r.Context(), rejectReasonAuth, "")
		h.recordRequest(r.Context(), "", http.StatusUnauthorized)
		return false
	}
}

// matchUpstream finds the upstream + ReverseProxy for r.URL.Path.
// Returns (_, _, false) and writes 404 when no upstream matches.
func (h *Handler) matchUpstream(w http.ResponseWriter, r *http.Request) (config.Upstream, *httputil.ReverseProxy, bool) {
	upstream, ok := h.cfg.Match(r.URL.Path)
	if !ok {
		setCorrelationHeaders(w, r)
		http.NotFound(w, r)
		h.recordRejected(r.Context(), rejectReasonNotFound, "")
		h.recordRequest(r.Context(), "", http.StatusNotFound)
		return config.Upstream{}, nil, false
	}
	rp, ok := h.rps[upstream.Name]
	if !ok {
		setCorrelationHeaders(w, r)
		http.NotFound(w, r)
		h.recordRejected(r.Context(), rejectReasonNotFound, upstream.Name)
		h.recordRequest(r.Context(), upstream.Name, http.StatusNotFound)
		return config.Upstream{}, nil, false
	}
	return upstream, rp, true
}

// admission bundles the per-upstream resources that ServeHTTP has
// claimed on behalf of a request: the breaker probe slot (if any),
// and the concurrency semaphore slot (if any). release() returns
// every slot it took. The release() call is the only path that reports
// the final status code to the breaker.
type admission struct {
	// br is nil when no breaker is configured for the upstream.
	br *breaker
	// sem is nil when no concurrency cap is configured.
	sem chan struct{}
}

// admit runs the breaker admit check, then the concurrency semaphore
// try-acquire. Returns (_, false) and writes a 503 / 429 when either
// gate rejects. When admit returns (adm, true), the caller MUST call
// adm.release exactly once.
//
// Breaker admit happens BEFORE the concurrency semaphore: an open
// breaker should NOT consume a concurrency slot. The order also
// ensures release() runs in reverse — concurrency slot first
// (immediately on return), breaker.report last (status code in hand).
func (h *Handler) admit(w http.ResponseWriter, r *http.Request, upstream config.Upstream) (*admission, bool) {
	adm := &admission{br: h.breakers[upstream.Name]}
	ctx := r.Context()
	if adm.br != nil {
		ok, retryAfter := adm.br.admit()
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeProxyPlainError(w, r, http.StatusServiceUnavailable, "upstream circuit breaker open")
			h.recordRejected(ctx, rejectReasonBreakerOpen, upstream.Name)
			h.recordRequest(ctx, upstream.Name, http.StatusServiceUnavailable)
			return nil, false
		}
	}
	if sem := h.upstreamSem[upstream.Name]; sem != nil {
		select {
		case sem <- struct{}{}:
			adm.sem = sem
		default:
			// Roll back the breaker admission so the probe slot
			// doesn't leak into the half-open state — concurrency
			// rejection is a no-upstream-contact event, not a probe
			// outcome.
			if adm.br != nil {
				adm.br.report(http.StatusTooManyRequests)
			}
			w.Header().Set("Retry-After", "1")
			writeProxyPlainError(w, r, http.StatusTooManyRequests, "upstream concurrency limit reached")
			h.recordRejected(ctx, rejectReasonConcurrency, upstream.Name)
			h.recordRequest(ctx, upstream.Name, http.StatusTooManyRequests)
			return nil, false
		}
	}
	return adm, true
}

// release returns the concurrency slot to the semaphore (if any) and
// reports the captured status code to the breaker (if any). Called
// exactly once per admission via ServeHTTP's `defer adm.release(cw)`.
//
// status comes from the statusCaptureWriter that wraps w; a missing
// status (i.e. zero) is mapped to 502 because a request that admitted
// but produced no response is, from the breaker's perspective, an
// upstream-side fault.
func (a *admission) release(cw *statusCaptureWriter) {
	if a.sem != nil {
		<-a.sem
	}
	if a.br != nil {
		code := cw.status
		if code == 0 {
			code = http.StatusBadGateway
		}
		a.br.report(code)
	}
}

// dispatch is the post-admission body of ServeHTTP. It either forwards
// transparently (no enrichment) or runs the full enriched pipeline.
// Every exit returns to ServeHTTP, where adm.release() folds the
// captured status code into the breaker.
func (h *Handler) dispatch(w *statusCaptureWriter, r *http.Request, upstream config.Upstream, rp *httputil.ReverseProxy) {
	prv := h.providers[upstream.Provider]
	op := prv.OperationFor(r.URL.Path)

	if op == "" {
		// Path is owned by this upstream but isn't a recognised LLM
		// endpoint (e.g. /v1/files): forward without enrichment.
		// The status captured by w flows into release()'s
		// breaker.report so the half-open probe slot still clears.
		rp.ServeHTTP(w, r)
		return
	}

	h.inflight.Add(r.Context(), 1, metric.WithAttributes(attribute.String("upstream", upstream.Name)))
	defer h.inflight.Add(r.Context(), -1, metric.WithAttributes(attribute.String("upstream", upstream.Name)))

	// Hard-cap pre-check: if the caller advertises a body above the
	// configured ceiling, refuse before we drain any bytes. The 413
	// status flows into release() via the wrapper.
	if hardCap := h.cfg.Request.MaxBodyBytes; hardCap > 0 && r.ContentLength > hardCap {
		writeProxyJSONError(w, r, http.StatusRequestEntityTooLarge, "request body exceeds maximum", "body_too_large")
		h.recordRejected(r.Context(), rejectReasonBodyTooLarge, upstream.Name)
		return
	}

	originalContentLength := r.ContentLength
	body, err := captureHeadAndForward(r, h.cfg.Request.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeProxyJSONError(w, r, http.StatusRequestEntityTooLarge, "request body exceeds maximum", "body_too_large")
			h.recordRejected(r.Context(), rejectReasonBodyTooLarge, upstream.Name)
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
		CaptureEvents: h.cfg.Content.Mode == config.CaptureEvents,
		CaptureLogs:   h.cfg.Content.Mode == config.CaptureLogs,
		Logger:        h.contentLogger,
		Redact:        redact.Func(redact.Profile(h.cfg.Content.Redact)),
	}
	info := prv.ParseRequest(span, r.URL.Path, body, contentOpts)
	if caller, _ := r.Context().Value(ctxKeyCaller).(string); caller != "" {
		info.Caller = caller
		span.SetAttributes(attribute.String(genai.AttrCaller, caller))
	}

	// The fallback closure lets finalize recover the status that the
	// ErrorHandler wrote when ModifyResponse never ran (upstream dial
	// failure, mid-body abort) — previously those spans/metrics
	// recorded status_code=0, blinding operators in exactly the
	// upstream-down scenario they most need telemetry for.
	finalize, modify := h.responseInterceptor(prv, &info, contentOpts, span, upstream.Name,
		replay.Entry{
			Method: r.Method, Path: r.URL.RequestURI(), Provider: prv.System(),
			Request: string(body),
			RequestTruncated: originalContentLength > int64(len(body)) ||
				(originalContentLength < 0 && len(body) == maxEnrichmentBodyBytes),
		},
		func() int { return w.status })
	ctx = context.WithValue(ctx, ctxKeyModify, modify)
	rp.ServeHTTP(w, r.WithContext(ctx))

	// finalize covers the non-streaming path and any 4xx/5xx where the
	// streaming wrapper never closed. It is idempotent.
	finalize(ctx)
}

// statusCaptureWriter records the first status code written to the
// response so adm.release() can fold it into the breaker after the
// handler returns. Without this wrapper, the transparent-passthrough
// paths in dispatch() would never tell the breaker what happened, and
// a half-open probe slot would leak forever.
//
// The wrapper preserves http.Flusher (httputil.ReverseProxy uses it
// when FlushInterval=-1) and http.Hijacker so streaming responses
// still flush per-write. http.CloseNotifier is intentionally NOT
// preserved — it has been deprecated since Go 1.11.
type statusCaptureWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusCaptureWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCaptureWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		// stdlib semantics: a Write before WriteHeader implies 200.
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

// Flush forwards to the underlying writer when it supports
// http.Flusher. httputil.ReverseProxy needs this for FlushInterval=-1
// streaming.
func (s *statusCaptureWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer so callers that type-assert
// for http.Hijacker or other optional interfaces still see them.
// Go 1.20+ http stack honours Unwrap for ResponseController.
func (s *statusCaptureWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// responseInterceptor builds a requestObserver and returns its
// Finalize / ModifyResponse method values:
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
	entry replay.Entry,
	fallbackStatus func() int,
) (finalize func(context.Context), modify modifyFn) {
	o := newRequestObserver(h, prv, info, content, span, upstreamName, fallbackStatus)
	o.record = entry
	return o.Finalize, o.ModifyResponse
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

func (h *Handler) callerForRequest(r *http.Request) string {
	if header := h.cfg.Attribution.Header; header != "" {
		value := strings.TrimSpace(r.Header.Get(header))
		r.Header.Del(header)
		if value != "" {
			return truncateUTF8(value, 128)
		}
	}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		if cn := strings.TrimSpace(r.TLS.PeerCertificates[0].Subject.CommonName); cn != "" {
			return "mtls:" + truncateUTF8(cn, 120)
		}
	}
	if h.auth.Enabled() {
		raw := strings.TrimSpace(r.Header.Get(h.authHeader))
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
		if raw != "" {
			sum := sha256.Sum256([]byte(raw))
			return "token:" + hex.EncodeToString(sum[:8])
		}
	}
	return ""
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

// authVerdict captures the auth gate's decision for a request. The
// proxy translates each verdict into the appropriate HTTP response
// without re-examining the credential.
type authVerdict int

const (
	authOK       authVerdict = iota // verified — admit
	authRejected                    // missing / wrong / malformed token
	authBusy                        // argon2 semaphore saturated — 429
)

// checkAuth pulls the bearer token out of the configured request
// header, strips the optional "Bearer " prefix, and asks the verifier
// to constant-time match it. Also strips the header from the inbound
// request so it does not get forwarded to upstream (the verified
// token is llmtap's secret, not the LLM provider's).
func (h *Handler) checkAuth(r *http.Request) authVerdict {
	raw := r.Header.Get(h.authHeader)
	r.Header.Del(h.authHeader)
	if raw == "" {
		return authRejected
	}
	// Accept either `Bearer <token>` or a bare token, so operators
	// can choose between matching standard reverse-proxy convention
	// and using a custom header naked.
	raw = strings.TrimPrefix(raw, "Bearer ")
	ok, busy := h.auth.Verify(raw)
	switch {
	case ok:
		return authOK
	case busy:
		return authBusy
	default:
		return authRejected
	}
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
