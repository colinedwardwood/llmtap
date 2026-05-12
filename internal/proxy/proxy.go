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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/colinedwardwood/llmtap/internal/auth"
	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/genai"
	"github.com/colinedwardwood/llmtap/internal/labels"
	"github.com/colinedwardwood/llmtap/internal/pricing"
	"github.com/colinedwardwood/llmtap/internal/provider"
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
}

// New builds a Handler from validated config and a Providers bundle. It
// returns an error if any upstream URL fails to parse or its provider is
// missing from the registry.
func New(cfg config.Config, providers provider.Registry, prov telemetry.Providers, logger *slog.Logger) (*Handler, error) {
	rps := make(map[string]*httputil.ReverseProxy, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		if _, ok := providers[u.Provider]; !ok {
			return nil, fmt.Errorf("provider %q not registered (upstream %q)", u.Provider, u.Name)
		}
		target, err := url.Parse(u.Target)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: parse target: %w", u.Name, err)
		}
		rp := &httputil.ReverseProxy{
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
		cfg:        cfg,
		providers:  providers,
		rps:        rps,
		tracer:     prov.Tracer,
		meters:     prov.Meters,
		inflight:   infl,
		requests:   reqs,
		logger:     logger,
		modelLabel: labels.NewModelLabel(labels.DefaultMaxCardinality),
		pricing:    priceTable,
		auth:       verifier,
		authHeader: cfg.Auth.HeaderName(),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	captureContent := h.cfg.Content.Mode == config.CaptureEvents
	info := prv.ParseRequest(span, r.URL.Path, body, captureContent)

	finalize, modify := h.responseInterceptor(prv, &info, captureContent, span, upstream.Name)
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
	captureContent bool,
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
			if captureContent {
				span.SetAttributes(attribute.String(
					"http.response.body_snippet",
					truncateUTF8(string(errCapture.head), 1024),
				))
			}
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
			h.meters.CostUSD.Add(ctx, usd, metric.WithAttributes(
				attribute.String(genai.AttrSystem, info.System),
				attribute.String(genai.AttrRequestModel, reqModelLabel),
				attribute.String(genai.AttrResponseModel, respModelLabel),
			))
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
			errCapture = &snippetCapture{headCap: 1024}
			origCloser := resp.Body
			resp.Body = struct {
				io.Reader
				io.Closer
			}{
				Reader: io.TeeReader(resp.Body, errCapture),
				Closer: origCloser,
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
			resp.Body = prv.WrapStream(span, info, resp.Body, captureContent,
				func() {}, // first-token bookkeeping is internal to provider
				func() {
					h.activeStreams.Add(-1)
					finalize(streamCtx)
				},
			)
			return nil
		}

		// Non-streaming JSON: read, parse, restore.
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		prv.ParseResponseJSON(span, info, raw, captureContent)
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		resp.ContentLength = int64(len(raw))
		resp.Header.Del("Content-Length")
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(raw)))
		return nil
	}
	return finalize, modify
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
	if strings.HasPrefix(raw, "Bearer ") {
		raw = strings.TrimPrefix(raw, "Bearer ")
	}
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
