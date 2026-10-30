package telemetry

import (
	"context"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/config"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
)

func TestNewGenAIMetersRegistersAllInstruments(t *testing.T) {
	t.Parallel()
	m := noop.NewMeterProvider().Meter("test")
	got, err := NewGenAIMeters(m)
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenUsage == nil || got.OperationDuration == nil || got.TimeToFirstToken == nil ||
		got.TimeToFirstByte == nil || got.CostUSD == nil || got.CostUSDTotal == nil {
		t.Fatalf("incomplete meters: %+v", got)
	}
}

func TestBuildResourceServiceAttrs(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Service.Name = "llmtap-test"
	cfg.Service.Namespace = "ns"
	cfg.Service.Env = "ci"
	res, err := buildResource(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nil resource")
	}
}

func TestPropagatorExcludesBaggage(t *testing.T) {
	t.Parallel()
	// Setup registers TraceContext only (D29). W3C baggage from clients
	// must not enter the active context via the global propagator.
	p := propagation.TraceContext{}
	carrier := propagation.MapCarrier{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"baggage":     "userId=alice",
	}
	ctx := p.Extract(context.Background(), carrier)
	bag := baggage.FromContext(ctx)
	if len(bag.Members()) != 0 {
		t.Fatal("TraceContext propagator must not extract baggage members")
	}
}
