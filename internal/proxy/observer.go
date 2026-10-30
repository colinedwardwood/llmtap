package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/colinedwardwood/llmtap/internal/genai"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/replay"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// requestObserver owns per-request enrichment: ModifyResponse wiring,
// metric recording, and span finalization. Extracted from the old
// responseInterceptor closure pyramid so finalize / metrics / status-
// recovery paths are unit-testable without standing up a ReverseProxy.
//
// Concurrency invariant (documented, not atomicized field-by-field):
//
//   - ModifyResponse runs at most once per request, and ReverseProxy
//     calls it before any bytes of the response body are copied to
//     the client. So statusCode / errCapture are exclusively written
//     here (or later by Finalize when status is still 0).
//   - Finalize may be invoked from the body Close/EOF callback and
//     again from ServeHTTP after ReverseProxy returns. Those two
//     calls race; finalized CompareAndSwap makes Finalize idempotent.
//   - info fields are mutated by the provider parsers during body
//     reads, which complete before (or as part of) the Finalize that
//     wins the CAS. The post-ServeHTTP Finalize is always a no-op by
//     then, or is the sole Finalize when ModifyResponse never ran.
//
// Do not start new writer goroutines against these fields without
// revisiting this invariant — that is what made the closure form
// untestable.
type requestObserver struct {
	h              *Handler
	prv            provider.Provider
	info           *provider.Info
	content        provider.ContentOpts
	span           trace.Span
	upstreamName   string
	fallbackStatus func() int

	finalized  atomic.Bool
	statusCode int
	// errCapture, when non-nil, indicates the response was an error
	// (4xx/5xx). It tees a snippet from the body without buffering
	// the whole thing so the original stream forwards intact.
	errCapture *snippetCapture
	record     replay.Entry
}

// newRequestObserver constructs an observer bound to an in-flight
// request. The returned Finalize / ModifyResponse method values are
// what ResponseInterceptor hands to ReverseProxy and ServeHTTP.
func newRequestObserver(
	h *Handler,
	prv provider.Provider,
	info *provider.Info,
	content provider.ContentOpts,
	span trace.Span,
	upstreamName string,
	fallbackStatus func() int,
) *requestObserver {
	return &requestObserver{
		h:              h,
		prv:            prv,
		info:           info,
		content:        content,
		span:           span,
		upstreamName:   upstreamName,
		fallbackStatus: fallbackStatus,
	}
}

// Finalize records metrics and ends the span exactly once. Safe to
// call from both the streaming/error body close path and the
// post-ServeHTTP path.
func (o *requestObserver) Finalize(ctx context.Context) {
	if !o.finalized.CompareAndSwap(false, true) {
		return
	}
	if o.info.Finished.IsZero() {
		o.info.Finished = time.Now()
	}
	// ModifyResponse never ran (upstream error path): recover the
	// status the ErrorHandler wrote to the client; if even that is
	// absent, the request admitted but produced no response — an
	// upstream-side fault, same mapping release() uses.
	if o.statusCode == 0 && o.fallbackStatus != nil {
		o.statusCode = o.fallbackStatus()
	}
	if o.statusCode == 0 {
		o.statusCode = http.StatusBadGateway
	}
	o.recordMetrics(ctx)
	o.logSummary(ctx)
	o.record.Status = o.statusCode
	o.record.Stream = o.info.Stream
	if err := o.h.recorder.Append(o.record); err != nil && o.h.logger != nil {
		o.h.logger.ErrorContext(ctx, "record request", slog.Any("err", err))
	}
	o.finishSpan()
}

func (o *requestObserver) logSummary(ctx context.Context) {
	if o.h.logger == nil {
		return
	}
	model := o.info.ResponseModel
	if model == "" {
		model = o.info.RequestModel
	}
	args := []any{
		slog.String("system", o.info.System),
		slog.String("operation", o.info.Operation),
		slog.String("model", model),
		slog.String("upstream", o.upstreamName),
		slog.Int("status", o.statusCode),
		slog.Int64("input_tokens", o.info.InputTokens),
		slog.Int64("output_tokens", o.info.OutputTokens),
		slog.Duration("duration", o.info.Finished.Sub(o.info.Started)),
	}
	if o.info.Caller != "" {
		args = append(args, slog.String("caller", o.info.Caller))
	}
	if usd, ok := o.h.pricing.CostDetailed(
		o.info.System, model, o.info.InputTokens, o.info.OutputTokens,
		o.info.CacheReadInputTokens, o.info.CacheCreationInputTokens,
	); ok {
		args = append(args, slog.Float64("cost_usd", usd))
	}
	o.h.logger.InfoContext(ctx, "llm request completed", args...)
}

func (o *requestObserver) finishSpan() {
	if o.statusCode >= 400 {
		o.span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", o.statusCode))
		if o.info.ErrorType == "" {
			o.info.ErrorType = strconv.Itoa(o.statusCode)
		}
		o.span.SetAttributes(attribute.String(genai.AttrErrorType, o.info.ErrorType))
	} else if o.statusCode > 0 {
		o.span.SetStatus(codes.Ok, "")
	}
	o.span.SetAttributes(attribute.Int("http.response.status_code", o.statusCode))
	if o.errCapture != nil {
		o.span.SetAttributes(attribute.Int("http.response.body_size", o.errCapture.total))
		if o.content.Capture() {
			// Redact BEFORE truncating: truncating first can slice
			// a secret so its tail no longer matches any redaction
			// pattern, shipping the surviving prefix into the
			// span. The capture buffer is 2x the snippet size so a
			// secret straddling the 1 KiB boundary is still seen
			// whole by the redactor.
			o.span.SetAttributes(attribute.String(
				"http.response.body_snippet",
				truncateUTF8(o.content.Clean(string(o.errCapture.head)), 1024),
			))
		}
	}
	// The circuit breaker is fed from ServeHTTP's admission release
	// path — NOT from here. release() reads the final status code
	// that reached the client via the statusCaptureWriter wrapper
	// and folds it into the breaker. That central reporting is the
	// only thing that prevents transparent-passthrough and
	// hard-cap-reject paths from leaking the half-open probe slot.
	o.span.End()
}

func (o *requestObserver) recordMetrics(ctx context.Context) {
	// Bound model-label cardinality before it hits any metric. Pricing
	// uses the raw model so snapshot suffixes still match the table.
	reqModelLabel := o.h.modelLabel.Normalize(o.info.RequestModel)
	respModelLabel := o.h.modelLabel.Normalize(o.info.ResponseModel)
	callerLabel := o.h.callerLabel.Normalize(o.info.Caller)
	commonAttrs := []attribute.KeyValue{
		attribute.String(genai.AttrSystem, o.info.System),
		attribute.String(genai.AttrOperationName, o.info.Operation),
		attribute.String(genai.AttrRequestModel, reqModelLabel),
		attribute.String("upstream", o.upstreamName),
		attribute.Int("http.response.status_code", o.statusCode),
	}
	if callerLabel != "" {
		commonAttrs = append(commonAttrs, attribute.String(genai.AttrCaller, callerLabel))
	}
	attrs := metric.WithAttributes(commonAttrs...)
	if d := o.info.DurationSeconds(); d > 0 {
		o.h.meters.OperationDuration.Record(ctx, d, attrs)
	}
	if t := o.info.TimeToFirstByteSeconds(); t > 0 {
		o.h.meters.TimeToFirstByte.Record(ctx, t, attrs)
		o.span.SetAttributes(attribute.Float64(genai.AttrTimeToFirstByte, t))
	}
	if t := o.info.TimeToFirstTokenSeconds(); t > 0 {
		o.h.meters.TimeToFirstToken.Record(ctx, t, attrs)
		o.span.SetAttributes(attribute.Float64(genai.AttrTimeToFirstToken, t))
	}
	if o.info.InputTokens > 0 {
		tokenAttrs := []attribute.KeyValue{
			attribute.String(genai.AttrSystem, o.info.System),
			attribute.String(genai.AttrOperationName, o.info.Operation),
			attribute.String(genai.AttrRequestModel, reqModelLabel),
			attribute.String("token_type", genai.TokenTypeInput),
		}
		if callerLabel != "" {
			tokenAttrs = append(tokenAttrs, attribute.String(genai.AttrCaller, callerLabel))
		}
		o.h.meters.TokenUsage.Record(ctx, o.info.InputTokens, metric.WithAttributes(tokenAttrs...))
	}
	if o.info.OutputTokens > 0 {
		tokenAttrs := []attribute.KeyValue{
			attribute.String(genai.AttrSystem, o.info.System),
			attribute.String(genai.AttrOperationName, o.info.Operation),
			attribute.String(genai.AttrRequestModel, reqModelLabel),
			attribute.String("token_type", genai.TokenTypeOutput),
		}
		if callerLabel != "" {
			tokenAttrs = append(tokenAttrs, attribute.String(genai.AttrCaller, callerLabel))
		}
		o.h.meters.TokenUsage.Record(ctx, o.info.OutputTokens, metric.WithAttributes(tokenAttrs...))
	}
	model := o.info.ResponseModel
	if model == "" {
		model = o.info.RequestModel
	}
	costAttrList := []attribute.KeyValue{
		attribute.String(genai.AttrSystem, o.info.System),
		attribute.String(genai.AttrRequestModel, reqModelLabel),
		attribute.String(genai.AttrResponseModel, respModelLabel),
	}
	if callerLabel != "" {
		costAttrList = append(costAttrList, attribute.String(genai.AttrCaller, callerLabel))
	}
	costAttrs := metric.WithAttributes(costAttrList...)
	if usd, ok := o.h.pricing.CostDetailed(
		o.info.System, model, o.info.InputTokens, o.info.OutputTokens,
		o.info.CacheReadInputTokens, o.info.CacheCreationInputTokens,
	); ok {
		// Per-call distribution + monotonic cumulative. The
		// histogram serves p50 / p95 cost dashboards; the
		// counter sidesteps float drift across millions of small adds.
		o.h.meters.CostUSD.Record(ctx, usd, costAttrs)
		o.h.meters.CostUSDTotal.Add(ctx, usd, costAttrs)
		o.span.SetAttributes(attribute.Float64(genai.AttrCostUSD, usd))
	} else if model != "" && (o.info.InputTokens > 0 || o.info.OutputTokens > 0) {
		// Token usage arrived but the catalogue has no rate for
		// this model — the FinOps dashboard just undercounted.
		o.h.pricingMisses.Add(ctx, 1, costAttrs)
	}
	// llmtap.requests.total is recorded once in ServeHTTP's defer so
	// passthrough and reject paths are counted too (D22).
}

// ModifyResponse is the ReverseProxy hook. Typed as a method so the
// context-value type assertion against modifyFn in Rewrite succeeds —
// named function types and their unnamed underlying type are distinct
// under runtime type assertions, and this method value has type
// modifyFn when assigned.
func (o *requestObserver) ModifyResponse(resp *http.Response) error {
	o.statusCode = resp.StatusCode
	o.info.FirstByteAt = time.Now()
	o.span.SetAttributes(attribute.String(genai.AttrSystem, o.info.System))

	if o.statusCode >= 400 {
		// Error bodies must reach the client byte-for-byte — they
		// frequently carry the structured detail the caller needs
		// to debug. We peel off a bounded snippet for the span via
		// an io.TeeReader; the rest of the bytes flow through
		// untouched. The snippet itself is gated on captureContent
		// since OpenAI/Anthropic error bodies routinely echo a
		// prefix of the offending API key, which would otherwise
		// leak into traces under the privacy-off default.
		//
		// Wrap the body in parseOnClose so Finalize fires on EOF —
		// before httputil's copy loop reports the body as drained.
		streamCtx := context.WithoutCancel(resp.Request.Context())
		// 2 KiB capture for a 1 KiB snippet: redaction runs on the
		// larger buffer so boundary-straddling secrets are masked
		// before the final truncation (see finishSpan).
		o.errCapture = &snippetCapture{headCap: 2048}
		origCloser := o.wrapIdleBody(resp.Body)
		teed := io.TeeReader(origCloser, o.errCapture)
		resp.Body = &parseOnClose{
			Reader: teed,
			closer: origCloser,
			onClose: func() {
				// Provider error envelopes carry stable error.type values.
				// Decode the bounded capture before finalization; content
				// remains gated/redacted by ContentOpts.
				o.prv.ParseResponseJSON(o.span, o.info, o.errCapture.head, o.content)
				o.record.Response = string(o.errCapture.head)
				o.record.ResponseTruncated = o.errCapture.total > len(o.errCapture.head)
				o.Finalize(streamCtx)
			},
		}
		return nil
	}

	if isEventStream(resp.Header) {
		o.h.incActiveStreams(1)
		o.info.Stream = true
		// Keep the request's trace context alive for the metric
		// emission that Finalize will do when the stream closes.
		// context.WithoutCancel preserves baggage/span context
		// while detaching from the request's cancellation chain —
		// metrics for a completed stream should fire even if the
		// client has already disconnected.
		streamCtx := context.WithoutCancel(resp.Request.Context())
		// Idle watchdog wraps the upstream body (inner) so silence
		// is measured on provider bytes, not on client drain pace.
		body := o.wrapIdleBody(resp.Body)
		var recordCallback *callbackReadCloser
		if o.h.recorder != nil {
			capture := &snippetCapture{headCap: maxEnrichmentBodyBytes}
			body = &teeReadCloser{Reader: io.TeeReader(body, capture), Closer: body}
			recordCallback = &callbackReadCloser{ReadCloser: body, beforeClose: func() {
				o.record.Response = string(capture.head)
				o.record.ResponseTruncated = capture.total > len(capture.head)
			}}
			body = recordCallback
		}
		resp.Body = o.prv.WrapStream(o.span, o.info, body, o.content,
			func() {
				if recordCallback != nil {
					recordCallback.run()
				}
				o.h.incActiveStreams(-1)
				o.Finalize(streamCtx)
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
	// Finalize both fire as soon as the upstream reader hits EOF.
	streamCtx := context.WithoutCancel(resp.Request.Context())
	capture := &snippetCapture{headCap: maxEnrichmentBodyBytes}
	origCloser := o.wrapIdleBody(resp.Body)
	teed := io.TeeReader(origCloser, capture)
	resp.Body = &parseOnClose{
		Reader: teed,
		closer: origCloser,
		onClose: func() {
			o.prv.ParseResponseJSON(o.span, o.info, capture.head, o.content)
			o.record.Response = string(capture.head)
			o.record.ResponseTruncated = capture.total > len(capture.head)
			o.Finalize(streamCtx)
		},
	}
	return nil
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

type callbackReadCloser struct {
	io.ReadCloser
	beforeClose func()
	once        atomic.Bool
}

func (c *callbackReadCloser) run() {
	if c.once.CompareAndSwap(false, true) && c.beforeClose != nil {
		c.beforeClose()
	}
}

func (c *callbackReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if err != nil {
		c.run()
	}
	return n, err
}

func (c *callbackReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.run()
	return err
}

// wrapIdleBody installs the post-header stream idle watchdog when
// http.stream_idle_timeout > 0. onIdle emits a span event + metric
// then Close()s the upstream body so in-flight Reads unblock.
func (o *requestObserver) wrapIdleBody(body io.ReadCloser) io.ReadCloser {
	idle := o.h.cfg.HTTP.StreamIdleTimeout
	if idle <= 0 {
		return body
	}
	return wrapIdleTimeout(body, idle, func() {
		o.span.AddEvent("llmtap.stream.idle_timeout",
			trace.WithAttributes(
				attribute.Float64("llmtap.stream.idle_timeout_seconds", idle.Seconds()),
				attribute.String("upstream", o.upstreamName),
			),
		)
		o.h.recordStreamIdleTimeout(o.upstreamName)
	})
}
