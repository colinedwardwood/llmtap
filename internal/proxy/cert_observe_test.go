package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestObserveCertRecordsNotAfterAndWarnsNearExpiry(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	meter := mp.Meter("cert-obs")
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	tokens, _ := meter.Int64Histogram("tu")
	dur, _ := meter.Float64Histogram("du")
	ttft, _ := meter.Float64Histogram("ttft")
	ttfb, _ := meter.Float64Histogram("ttfb")
	cost, _ := meter.Float64Histogram("cost")
	costTotal, _ := meter.Float64Counter("cost.total")

	var logBuf bytes.Buffer
	warnLogger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	h, err := New(config.Default(), provider.BuiltIn(), telemetry.Providers{
		Tracer: sdktrace.NewTracerProvider().Tracer("t"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage: tokens, OperationDuration: dur,
			TimeToFirstToken: ttft, TimeToFirstByte: ttfb,
			CostUSD: cost, CostUSDTotal: costTotal,
		},
		Shutdown: func(context.Context) error { return nil },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	notAfter := time.Now().Add(48 * time.Hour)
	h.observeCert(notAfter, warnLogger)
	if !bytes.Contains(logBuf.Bytes(), []byte("expires soon")) {
		t.Fatalf("expected near-expiry warning; log=%q", logBuf.String())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llmtap.tls.cert_not_after" {
				continue
			}
			g := m.Data.(metricdata.Gauge[int64])
			for _, dp := range g.DataPoints {
				if dp.Value == notAfter.Unix() {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("cert_not_after gauge missing expected unix value")
	}

	h.recordCertReloadFailure()
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var fails int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llmtap.tls.cert_reload_failures.total" {
				continue
			}
			sum := m.Data.(metricdata.Sum[int64])
			for _, dp := range sum.DataPoints {
				fails += dp.Value
			}
		}
	}
	if fails != 1 {
		t.Fatalf("reload failures = %d, want 1", fails)
	}
}
