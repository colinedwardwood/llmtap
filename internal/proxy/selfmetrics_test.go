package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/auth"
	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// selfMetricHarness builds a Handler whose meters land in a ManualReader
// so D22 self-metric assertions can inspect datapoints.
func selfMetricHarness(t *testing.T, cfg config.Config) (*Handler, *metric.ManualReader, *httptest.Server) {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	meter := mp.Meter("selfmetrics-test")
	tp := sdktrace.NewTracerProvider()

	tokens, _ := meter.Int64Histogram("gen_ai.client.token.usage")
	dur, _ := meter.Float64Histogram("gen_ai.client.operation.duration")
	ttft, _ := meter.Float64Histogram("gen_ai.client.time_to_first_token")
	ttfb, _ := meter.Float64Histogram("gen_ai.server.time_to_first_byte")
	cost, _ := meter.Float64Histogram("gen_ai.client.cost.usd")
	costTotal, _ := meter.Float64Counter("gen_ai.client.cost.usd.total")

	prov := telemetry.Providers{
		Tracer: tp.Tracer("test"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage: tokens, OperationDuration: dur,
			TimeToFirstToken: ttft, TimeToFirstByte: ttfb,
			CostUSD: cost, CostUSDTotal: costTotal,
		},
		Shutdown: func(context.Context) error { return nil },
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return h, reader, ts
}

func counterSum(t *testing.T, reader *metric.ManualReader, name string, attrs map[string]string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is not an int64 sum", name)
			}
			for _, dp := range sum.DataPoints {
				if attrsMatch(dp.Attributes, attrs) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

func gaugeValue(t *testing.T, reader *metric.ManualReader, name, upstream string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s is not an int64 gauge", name)
			}
			for _, dp := range g.DataPoints {
				if attrsMatch(dp.Attributes, map[string]string{"upstream": upstream}) {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

func attrsMatch(set attribute.Set, want map[string]string) bool {
	for k, v := range want {
		got, ok := set.Value(attribute.Key(k))
		if !ok {
			return false
		}
		switch got.Type() {
		case attribute.STRING:
			if got.AsString() != v {
				return false
			}
		case attribute.INT64:
			var n int64
			for _, c := range v {
				if c < '0' || c > '9' {
					return false
				}
				n = n*10 + int64(c-'0')
			}
			if got.AsInt64() != n {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func TestRequestsTotalCountsPassthrough(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	_, reader, ts := selfMetricHarness(t, cfg)

	// /v1/files is passthrough (not an enriched LLM op).
	resp, err := http.Get(ts.URL + "/v1/files")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := counterSum(t, reader, "llmtap.requests.total", map[string]string{
		"upstream":                  "openai",
		"http.response.status_code": "200",
	})
	if got != 1 {
		t.Fatalf("requests.total passthrough = %d, want 1", got)
	}
}

func TestRejectedAuthIncrementsCounters(t *testing.T) {
	t.Parallel()
	hash, err := auth.Hash("correct-secret")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should not be contacted on auth reject")
		w.WriteHeader(500)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.Auth.Tokens = []string{hash}
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	_, reader, ts := selfMetricHarness(t, cfg)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-LLMTAP-Token", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	if got := counterSum(t, reader, "llmtap.auth.rejected.total", map[string]string{}); got != 1 {
		t.Fatalf("auth.rejected.total = %d, want 1", got)
	}
	if got := counterSum(t, reader, "llmtap.requests.rejected.total", map[string]string{
		"reason": "auth", "upstream": "_none",
	}); got != 1 {
		t.Fatalf("rejected{auth} = %d, want 1", got)
	}
	if got := counterSum(t, reader, "llmtap.requests.total", map[string]string{
		"upstream": "_none", "http.response.status_code": "401",
	}); got != 1 {
		t.Fatalf("requests.total 401 = %d, want 1", got)
	}
}

func TestBreakerStateGaugeAndTripLog(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 2, Window: time.Minute, RecoveryWindow: time.Minute},
	}}
	_, reader, ts := selfMetricHarness(t, cfg)

	body := []byte(`{"model":"gpt-5-mini","messages":[{"role":"user","content":"x"}]}`)
	for i := 0; i < 2; i++ {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	val, ok := gaugeValue(t, reader, "llmtap.breaker.state", "openai")
	if !ok || val != breakerGaugeOpen {
		t.Fatalf("breaker.state = %d (ok=%v), want open=%d", val, ok, breakerGaugeOpen)
	}

	// Next request should be short-circuited 503 and counted as rejected.
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := counterSum(t, reader, "llmtap.requests.rejected.total", map[string]string{
		"reason": "breaker_open", "upstream": "openai",
	}); got != 1 {
		t.Fatalf("rejected{breaker_open} = %d, want 1", got)
	}
}
