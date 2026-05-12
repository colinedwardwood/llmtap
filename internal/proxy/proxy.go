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

// maxRequestBodyBytes caps the request body llmtap reads into memory before
// forwarding. Real LLM requests rarely exceed ~512 KiB; bodies above the cap
// are forwarded transparently without enrichment to avoid OOM in adversarial
// settings. Configurable in a future revision if needed.
const maxRequestBodyBytes = 4 * 1024 * 1024 // 4 MiB

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
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	body, err := readCappedBody(r)
	if err != nil {
		h.logger.WarnContext(r.Context(), "request body read failed; falling back to transparent proxy",
			slog.Any("err", err),
		)
		rp.ServeHTTP(w, r)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

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
//   - modify: the ReverseProxy.ModifyResponse hook.
func (h *Handler) responseInterceptor(
	prv provider.Provider,
	info *provider.Info,
	captureContent bool,
	span trace.Span,
	upstreamName string,
) (finalize func(context.Context), modify func(*http.Response) error) {
	var (
		finalized atomic.Bool
		statusCode int
	)

	finishSpan := func() {
		if statusCode >= 400 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", statusCode))
		} else if statusCode > 0 {
			span.SetStatus(codes.Ok, "")
		}
		span.SetAttributes(attribute.Int("http.response.status_code", statusCode))
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
		if usd, ok := pricing.Cost(info.System, model, info.InputTokens, info.OutputTokens); ok {
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
			// Error bodies are small and useful: read for the upstream
			// error code, restore the body, and let finalize record.
			body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			if err == nil {
				span.SetAttributes(attribute.String("http.response.body_snippet", truncateUTF8(string(body), 1024)))
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
			} else {
				resp.Body = http.NoBody
			}
			return nil
		}

		if isEventStream(resp.Header) {
			h.activeStreams.Add(1)
			info.Stream = true
			resp.Body = prv.WrapStream(span, info, resp.Body, captureContent,
				func() {}, // first-token bookkeeping is internal to provider
				func() {
					h.activeStreams.Add(-1)
					finalize(context.Background())
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

// readCappedBody reads at most maxRequestBodyBytes+1 to detect overflow and
// restores the original body for the proxy to forward.
func readCappedBody(r *http.Request) ([]byte, error) {
	limited := io.LimitReader(r.Body, maxRequestBodyBytes+1)
	body, err := io.ReadAll(limited)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxRequestBodyBytes {
		return nil, errors.New("request body exceeds maximum")
	}
	return body, nil
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

// WrapWithOTel returns the handler wrapped in otelhttp middleware so llmtap's
// own HTTP server is itself traced. Caller passes the route name.
func (h *Handler) WrapWithOTel(routeName string) http.Handler {
	return otelhttp.NewHandler(h, routeName,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}
