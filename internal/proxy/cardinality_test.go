package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/labels"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// realMeterProviders wires a Tracer plus a ManualReader-backed
// MeterProvider so the test can introspect every label set actually
// recorded into metrics. The fakeProviders helper used by other tests
// uses noop meters, which is fine for span assertions but useless for
// cardinality assertions. Returns the SpanRecorder too so a single
// helper covers both metric- and span-based assertions.
func realMeterProviders(t *testing.T) (telemetry.Providers, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := mp.Meter("test")

	tokens, err := meter.Int64Histogram("gen_ai.client.token.usage")
	if err != nil {
		t.Fatal(err)
	}
	dur, err := meter.Float64Histogram("gen_ai.client.operation.duration")
	if err != nil {
		t.Fatal(err)
	}
	ttft, err := meter.Float64Histogram("gen_ai.client.time_to_first_token")
	if err != nil {
		t.Fatal(err)
	}
	cost, err := meter.Float64Counter("gen_ai.client.cost.usd")
	if err != nil {
		t.Fatal(err)
	}
	return telemetry.Providers{
		Tracer: tp.Tracer("test"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage:        tokens,
			OperationDuration: dur,
			TimeToFirstToken:  ttft,
			CostUSD:           cost,
		},
		Shutdown: func(context.Context) error { return nil },
	}, rec, reader
}

// TestProxyMetricsModelCardinalityIsBounded fires far more distinct model
// strings through the proxy than the cardinality cap permits and asserts
// that the actually-recorded set of gen_ai.request.model label values
// never exceeds cap+1 ("_other" is the +1).
//
// This is the regression test for the FATAL FLAW in the round-2
// adversarial review: hostile/buggy callers minting unbounded metric
// series and exhausting the operator's o11y backend.
func TestProxyMetricsModelCardinalityIsBounded(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"x",
			"model":"gpt-4o-mini",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}
		}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, _, reader := realMeterProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Fire N requests with N distinct synthetic model names — many more
	// than the default cap.
	const totalDistinctModels = labels.DefaultMaxCardinality * 5
	for i := 0; i < totalDistinctModels; i++ {
		body := []byte(fmt.Sprintf(
			`{"model":"synthetic-model-%d","messages":[{"role":"user","content":"hi"}]}`, i,
		))
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	distinct := map[string]struct{}{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			collectModelLabels(m.Data, distinct)
		}
	}
	if len(distinct) == 0 {
		t.Fatal("no model label values were observed at all")
	}
	// Cap admits at most DefaultMaxCardinality distinct values; "_other"
	// covers the remainder. Allow cap+1 as the upper bound.
	if max := labels.DefaultMaxCardinality + 1; len(distinct) > max {
		t.Errorf("recorded %d distinct gen_ai.request.model label values; want <= %d", len(distinct), max)
	}
	// And the overflow bucket must exist — proves the cap engaged.
	if _, ok := distinct[labels.OtherLabel]; !ok {
		t.Errorf("expected overflow label %q to be present after firing %d distinct models", labels.OtherLabel, totalDistinctModels)
	}
}

// collectModelLabels walks a metricdata.Aggregation and pulls out every
// distinct value of the gen_ai.request.model attribute that appears on
// any data point.
func collectModelLabels(data metricdata.Aggregation, out map[string]struct{}) {
	switch d := data.(type) {
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			if v, ok := dp.Attributes.Value("gen_ai.request.model"); ok {
				out[v.AsString()] = struct{}{}
			}
		}
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			if v, ok := dp.Attributes.Value("gen_ai.request.model"); ok {
				out[v.AsString()] = struct{}{}
			}
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			if v, ok := dp.Attributes.Value("gen_ai.request.model"); ok {
				out[v.AsString()] = struct{}{}
			}
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			if v, ok := dp.Attributes.Value("gen_ai.request.model"); ok {
				out[v.AsString()] = struct{}{}
			}
		}
	}
}
