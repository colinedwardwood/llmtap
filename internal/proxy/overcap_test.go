package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	cost, _ := meter.Float64Counter("cost")
	return telemetry.Providers{
		Tracer: tp.Tracer("t"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage: tokens, OperationDuration: dur, TimeToFirstToken: ttft, CostUSD: cost,
		},
		Shutdown: func(context.Context) error { return nil },
	}
}

// Demonstrates that bodies > 4 MiB are silently truncated to empty
// when they hit the LLM endpoint path (chat completions).
func TestProxyOversizeBodyIsCorrupted(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	h, _ := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 5 MiB body (over 4 MiB cap) but valid JSON wrapping a long text.
	pad := strings.Repeat("a", 5*1024*1024)
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"` + pad + `"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	upstreamGot := <-got
	t.Logf("client status: %d, upstream got %d bytes (sent %d)", resp.StatusCode, len(upstreamGot), len(body))
	if len(upstreamGot) == 0 {
		t.Fatalf("BUG: upstream received empty body when client sent %d bytes", len(body))
	}
	if len(upstreamGot) != len(body) {
		t.Errorf("BUG: upstream received %d bytes; client sent %d", len(upstreamGot), len(body))
	}
}
