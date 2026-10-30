package provider

import (
	"context"

	"github.com/colinedwardwood/llmtap/internal/genai"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// ContentOpts shapes how providers handle prompt/completion content
// when populating span events or OTel log records.
type ContentOpts struct {
	// CaptureEvents attaches content as span events (content.mode: events).
	CaptureEvents bool
	// CaptureLogs emits content via the OTel log signal (content.mode:
	// logs) so collectors can route retention separately from traces.
	CaptureLogs bool
	// Logger is required when CaptureLogs is true. Nil is a no-op.
	Logger otellog.Logger
	// Redact, if non-nil, is applied to every captured content string
	// before it reaches a span attribute or log body.
	Redact func(string) string
}

// capturing reports whether any content sink is active.
func (o ContentOpts) capturing() bool {
	return o.CaptureEvents || o.CaptureLogs
}

// Capture reports whether prompt/completion content should be recorded.
func (o ContentOpts) Capture() bool { return o.capturing() }

// Clean returns s with the configured redactor applied, or s unchanged
// when Redact is nil.
func (o ContentOpts) Clean(s string) string {
	if o.Redact == nil {
		return s
	}
	return o.Redact(s)
}

// emitRoleMessage records a request message (system/user/assistant/tool).
func (o ContentOpts) emitRoleMessage(ctx context.Context, span trace.Span, eventName, role, content string) {
	if !o.capturing() || content == "" {
		return
	}
	cleaned := o.Clean(content)
	if o.CaptureEvents && span.IsRecording() {
		span.AddEvent(eventName, trace.WithAttributes(
			attribute.String("role", role),
			attribute.String("content", cleaned),
		))
	}
	if o.CaptureLogs && o.Logger != nil {
		var rec otellog.Record
		rec.SetBody(otellog.StringValue(cleaned))
		rec.AddAttributes(
			otellog.String("gen_ai.event", eventName),
			otellog.String("role", role),
			otellog.String("message", cleaned),
		)
		o.Logger.Emit(ctx, rec)
	}
}

// emitChoice records a completion choice.
func (o ContentOpts) emitChoice(ctx context.Context, span trace.Span, index int, finishReason, message string) {
	if !o.capturing() || message == "" {
		return
	}
	cleaned := o.Clean(message)
	if o.CaptureEvents && span.IsRecording() {
		span.AddEvent(genai.EventChoice, trace.WithAttributes(
			attribute.Int("index", index),
			attribute.String("finish_reason", finishReason),
			attribute.String("message", cleaned),
		))
	}
	if o.CaptureLogs && o.Logger != nil {
		var rec otellog.Record
		rec.SetBody(otellog.StringValue(cleaned))
		rec.AddAttributes(
			otellog.String("gen_ai.event", genai.EventChoice),
			otellog.Int("index", index),
			otellog.String("finish_reason", finishReason),
			otellog.String("message", cleaned),
		)
		o.Logger.Emit(ctx, rec)
	}
}

// emitAssembledChoice records streamed completion text at stream close.
func (o ContentOpts) emitAssembledChoice(ctx context.Context, span trace.Span, message string, truncated bool) {
	if !o.capturing() || message == "" {
		return
	}
	cleaned := o.Clean(message)
	if o.CaptureEvents && span.IsRecording() {
		span.AddEvent(genai.EventChoice, trace.WithAttributes(
			attribute.String("message", cleaned),
		))
		if truncated {
			span.SetAttributes(attribute.Bool("llmtap.content_truncated", true))
		}
	}
	if o.CaptureLogs && o.Logger != nil {
		var rec otellog.Record
		rec.SetBody(otellog.StringValue(cleaned))
		rec.AddAttributes(
			otellog.String("gen_ai.event", genai.EventChoice),
			otellog.String("message", cleaned),
		)
		if truncated {
			rec.AddAttributes(otellog.Bool("llmtap.content_truncated", true))
		}
		o.Logger.Emit(ctx, rec)
	}
}
