package telemetry

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestGateStartsNotReadyUntilExport(t *testing.T) {
	t.Parallel()
	g := NewGate()
	if g.Ready() {
		t.Fatal("new gate must start not-ready")
	}
	g.MarkExportOK()
	if !g.Ready() {
		t.Fatal("gate must be ready after successful export")
	}
	g.BeginDrain()
	if g.Ready() {
		t.Fatal("gate must go not-ready on drain")
	}
}

func TestExportOKSpanExporter(t *testing.T) {
	t.Parallel()
	g := NewGate()
	inner := &stubSpanExporter{}
	exp := &exportOKSpanExporter{SpanExporter: inner, gate: g}

	if err := exp.ExportSpans(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !g.Ready() {
		t.Fatal("successful ExportSpans must flip the gate")
	}
}

func TestExportOKSpanExporterKeepsGateOnError(t *testing.T) {
	t.Parallel()
	g := NewGate()
	inner := &stubSpanExporter{err: errors.New("collector down")}
	exp := &exportOKSpanExporter{SpanExporter: inner, gate: g}

	if err := exp.ExportSpans(context.Background(), nil); err == nil {
		t.Fatal("expected export error")
	}
	if g.Ready() {
		t.Fatal("failed export must not flip the gate")
	}
}

func TestExportOKMetricExporter(t *testing.T) {
	t.Parallel()
	g := NewGate()
	inner := &stubMetricExporter{}
	exp := &exportOKMetricExporter{Exporter: inner, gate: g}

	if err := exp.Export(context.Background(), &metricdata.ResourceMetrics{}); err != nil {
		t.Fatal(err)
	}
	if !g.Ready() {
		t.Fatal("successful metric Export must flip the gate")
	}
}

type stubSpanExporter struct{ err error }

func (s *stubSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return s.err
}
func (s *stubSpanExporter) Shutdown(context.Context) error { return nil }

type stubMetricExporter struct{ err error }

func (s *stubMetricExporter) Temporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}
func (s *stubMetricExporter) Aggregation(sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return nil
}
func (s *stubMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return s.err
}
func (s *stubMetricExporter) ForceFlush(context.Context) error { return nil }
func (s *stubMetricExporter) Shutdown(context.Context) error   { return nil }
