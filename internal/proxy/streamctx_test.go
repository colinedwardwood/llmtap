package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestStreamingMetricsCarryTraceContext is the regression test for A5:
// streaming responses run their `finalize` from a `WrapStream`
// onClose callback, which previously passed `context.Background()` —
// stripping the per-request trace context off every metric for the
// dominant streaming case. With the fix, the metric exemplar's TraceID
// must equal the span's TraceID, so Grafana's
// metric → trace exemplar jump actually works.
func TestStreamingMetricsCarryTraceContext(t *testing.T) {
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
	prov, rec, reader := realMeterProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, logger)
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
	// Drain the stream so onClose fires.
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Find the streaming span and pin its TraceID.
	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded")
	}
	var wantTraceID string
	for _, s := range spans {
		if s.Name() == "chat gpt-4o-mini" {
			wantTraceID = s.SpanContext().TraceID().String()
		}
	}
	if wantTraceID == "" {
		t.Fatal("streaming chat span not found")
	}

	// Collect metric data and look for any histogram exemplar tied to
	// the span. The duration histogram is the canonical observation;
	// TTFT is also recorded but may have no exemplar if the stream
	// completed within the same instant.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	exemplarTrace := ""
nextMetric:
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			d, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, dp := range d.DataPoints {
				for _, ex := range dp.Exemplars {
					if id := traceIDString(ex.TraceID); id != "" {
						exemplarTrace = id
						break nextMetric
					}
				}
			}
		}
	}
	if exemplarTrace == "" {
		t.Fatal("no metric exemplar carried a trace context — streaming finalize is still using context.Background()")
	}
	if exemplarTrace != wantTraceID {
		t.Errorf("exemplar trace = %q, want span trace = %q", exemplarTrace, wantTraceID)
	}
}

// traceIDString formats a raw 16-byte trace ID as lower-hex if any
// byte is non-zero.
func traceIDString(id []byte) string {
	for _, b := range id {
		if b != 0 {
			const hex = "0123456789abcdef"
			out := make([]byte, len(id)*2)
			for i, x := range id {
				out[i*2] = hex[x>>4]
				out[i*2+1] = hex[x&0x0f]
			}
			return string(out)
		}
	}
	return ""
}
