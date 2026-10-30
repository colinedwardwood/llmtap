package telemetry

import (
	"context"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/genai"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestPriorityExporterKeepsErrorsAndExpensiveSpans(t *testing.T) {
	t.Parallel()
	sink := tracetest.NewInMemoryExporter()
	priority := &prioritySpanExporter{
		SpanExporter:  sink,
		ratio:         0,
		costThreshold: 0.01,
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(priority))
	tracer := tp.Tracer("test")

	_, normal := tracer.Start(context.Background(), "normal")
	normal.End()

	_, expensive := tracer.Start(context.Background(), "expensive")
	expensive.SetAttributes(attribute.Float64(genai.AttrCostUSD, 0.02))
	expensive.End()

	_, failed := tracer.Start(context.Background(), "failed")
	failed.SetStatus(codes.Error, "upstream failed")
	failed.End()

	spans := sink.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported %d spans, want 2", len(spans))
	}
	if spans[0].Name != "expensive" || spans[1].Name != "failed" {
		t.Fatalf("exported spans = %s, %s", spans[0].Name, spans[1].Name)
	}
}
