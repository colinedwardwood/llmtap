package telemetry

import (
	"context"
	"sync/atomic"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Gate is the two-latch readiness signal for /readyz:
//
//  1. exportOK flips true on the first successful OTLP export
//     (trace or metric). Until then the process is up but not
//     proven able to ship telemetry — rolling updates must not
//     send it traffic.
//  2. draining flips true on SIGTERM / ctx cancel so /readyz goes
//     503 before graceful drain, giving the load balancer time to
//     stop routing (pair with a k8s preStop / terminationGracePeriod).
//
// Ready is true only when exportOK && !draining.
type Gate struct {
	exportOK atomic.Bool
	draining atomic.Bool
}

// NewGate returns a Gate that starts not-ready.
func NewGate() *Gate { return &Gate{} }

// Ready reports whether /readyz should return 200.
func (g *Gate) Ready() bool {
	return g != nil && g.exportOK.Load() && !g.draining.Load()
}

// MarkExportOK records a successful OTLP export. Idempotent.
func (g *Gate) MarkExportOK() {
	if g != nil {
		g.exportOK.Store(true)
	}
}

// BeginDrain marks the process as terminating. Idempotent. Call as
// soon as SIGTERM is received, before waiting for in-flight work.
func (g *Gate) BeginDrain() {
	if g != nil {
		g.draining.Store(true)
	}
}

// exportOKSpanExporter wraps a SpanExporter and flips the gate on
// the first successful ExportSpans.
type exportOKSpanExporter struct {
	sdktrace.SpanExporter
	gate *Gate
}

func (e *exportOKSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	if err == nil {
		e.gate.MarkExportOK()
	}
	return err
}

// exportOKMetricExporter wraps a metric Exporter and flips the gate
// on the first successful Export.
type exportOKMetricExporter struct {
	sdkmetric.Exporter
	gate *Gate
}

func (e *exportOKMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err == nil {
		e.gate.MarkExportOK()
	}
	return err
}
