package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// fakeProviders gives us a Tracer with a SpanRecorder and no-op Meters so
// the proxy can run without an OTLP backend in tests.
func fakeProviders(t *testing.T) (telemetry.Providers, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	meter := noop.NewMeterProvider().Meter("noop")

	tokens, _ := meter.Int64Histogram("gen_ai.client.token.usage")
	dur, _ := meter.Float64Histogram("gen_ai.client.operation.duration")
	ttft, _ := meter.Float64Histogram("gen_ai.client.time_to_first_token")
	cost, _ := meter.Float64Histogram("gen_ai.client.cost.usd")
	costTotal, _ := meter.Float64Counter("gen_ai.client.cost.usd.total")

	return telemetry.Providers{
		Tracer: tp.Tracer("test"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage:        tokens,
			OperationDuration: dur,
			TimeToFirstToken:  ttft,
			CostUSD:           cost,
			CostUSDTotal:      costTotal,
		},
		Shutdown: func(context.Context) error { return nil },
	}, rec
}

func TestProxyEndToEndNonStreaming(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify llmtap forwarded the body unchanged.
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"gpt-4o-mini"`) {
			t.Errorf("upstream got unexpected body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"resp-1",
			"model":"gpt-4o-mini-2024-07-18",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
			"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}
		}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, rec := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"model": "gpt-4o-mini",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}

	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded")
	}

	var found bool
	for _, s := range spans {
		if s.Name() == "chat gpt-4o-mini" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, len(spans))
		for i, s := range spans {
			names[i] = s.Name()
		}
		t.Fatalf("did not find genai span; got %v", names)
	}
}

func TestProxyEndToEndStreaming(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
			`{"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, rec := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if !strings.Contains(string(body), `"hi"`) || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("client did not receive intact SSE stream: %s", body)
	}

	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded")
	}
	var got bool
	for _, s := range spans {
		if s.Name() == "chat gpt-4o-mini" {
			got = true
			for _, a := range s.Attributes() {
				if string(a.Key) == "gen_ai.usage.output_tokens" && a.Value.AsInt64() != 2 {
					t.Errorf("output_tokens = %d", a.Value.AsInt64())
				}
			}
		}
	}
	if !got {
		t.Fatal("did not find streaming chat span")
	}
}

func TestProxyTransparentForUnknownEndpoint(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, rec := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/files")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(rec.Ended()) != 0 {
		t.Fatalf("expected no genai spans for transparent path, got %d", len(rec.Ended()))
	}
}
