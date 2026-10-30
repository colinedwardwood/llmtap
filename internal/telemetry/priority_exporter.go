package telemetry

import (
	"context"
	"encoding/binary"
	"math"
	"time"

	"github.com/colinedwardwood/llmtap/internal/genai"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
)

// prioritySpanExporter is a bounded, end-of-span sampling stage. Unlike a
// head sampler it can see outcome, latency, and computed USD cost. This is
// processor-style sampling, not true collector tail sampling: decisions are
// per span and state-free so memory use does not grow with active traces.
type prioritySpanExporter struct {
	trace.SpanExporter
	ratio         float64
	costThreshold float64
	slowThreshold time.Duration
}

func (e *prioritySpanExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	if e.ratio >= 1 && e.costThreshold <= 0 && e.slowThreshold <= 0 {
		return e.SpanExporter.ExportSpans(ctx, spans)
	}
	kept := make([]trace.ReadOnlySpan, 0, len(spans))
	for _, span := range spans {
		if e.keep(span) {
			kept = append(kept, span)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return e.SpanExporter.ExportSpans(ctx, kept)
}

func (e *prioritySpanExporter) keep(span trace.ReadOnlySpan) bool {
	if span.Status().Code == codes.Error {
		return true
	}
	for _, attr := range span.Attributes() {
		switch string(attr.Key) {
		case genai.AttrErrorType:
			if attr.Value.AsString() != "" {
				return true
			}
		case "http.response.status_code":
			if attr.Value.AsInt64() >= 500 {
				return true
			}
		case genai.AttrCostUSD:
			if e.costThreshold > 0 && attr.Value.AsFloat64() >= e.costThreshold {
				return true
			}
		}
	}
	if e.slowThreshold > 0 && span.EndTime().Sub(span.StartTime()) >= e.slowThreshold {
		return true
	}
	if e.ratio <= 0 {
		return false
	}
	id := span.SpanContext().TraceID()
	n := binary.BigEndian.Uint64(id[:8])
	return float64(n)/float64(math.MaxUint64) < e.ratio
}
