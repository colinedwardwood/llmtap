package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func fakeProv(t *testing.T) telemetry.Providers {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	meter := noop.NewMeterProvider().Meter("noop")
	tokens, _ := meter.Int64Histogram("tu")
	dur, _ := meter.Float64Histogram("du")
	ttft, _ := meter.Float64Histogram("ttft")
	cost, _ := meter.Float64Histogram("cost")
	costTotal, _ := meter.Float64Counter("cost.total")
	return telemetry.Providers{
		Tracer: tp.Tracer("t"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage: tokens, OperationDuration: dur, TimeToFirstToken: ttft, CostUSD: cost, CostUSDTotal: costTotal,
		},
		Shutdown: func(context.Context) error { return nil },
	}
}

// TestProxyOversizeBodyForwardsIntact is the regression test for A3:
// the proxy must forward every byte to the upstream even when the body
// exceeds the enrichment buffer. Enrichment may degrade (the parser
// only sees the first MiB); forwarding must not.
//
// Previously named TestProxyOversizeBodyIsCorrupted — it documented
// the bug at the same address. Renamed to reflect the fixed contract.
func TestProxyOversizeBodyForwardsIntact(t *testing.T) {
	t.Parallel()

	got := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 5 MiB body — over the enrichment cap, under the hard cap.
	pad := strings.Repeat("a", 5*1024*1024)
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"` + pad + `"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d (want 200)", resp.StatusCode)
	}

	upstreamGot := <-got
	if len(upstreamGot) != len(body) {
		t.Fatalf("upstream received %d bytes; client sent %d", len(upstreamGot), len(body))
	}
	if !bytes.Equal(upstreamGot, body) {
		t.Errorf("upstream body diverged from sent body (lengths match)")
	}
}

// TestProxyHardCapRejectsCleanly asserts that bodies above the
// configurable hard cap receive a clean 413 from the proxy with NO
// upstream call. The 502-by-corruption path A3 fixed should never
// engage; this test guards the boundary above which we explicitly
// refuse.
func TestProxyHardCapRejectsCleanly(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	// Tighten the hard cap to keep the test cheap.
	cfg.Request.MaxBodyBytes = 2 * 1024 * 1024
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 3 MiB > 2 MiB hard cap.
	body := bytes.Repeat([]byte{'a'}, 3*1024*1024)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
	if hits := upstreamHits.Load(); hits != 0 {
		t.Errorf("upstream was hit %d times; want 0 (hard-cap should reject before forwarding)", hits)
	}
}
