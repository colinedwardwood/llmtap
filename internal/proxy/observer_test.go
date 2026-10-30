package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/labels"
	"github.com/colinedwardwood/llmtap/internal/pricing"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// observerHarness builds a minimal Handler + span recorder so
// requestObserver paths can be exercised without standing up a
// ReverseProxy.
func observerHarness(t *testing.T) (*Handler, *tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	meter := noop.NewMeterProvider().Meter("noop")
	tokens, _ := meter.Int64Histogram("gen_ai.client.token.usage")
	dur, _ := meter.Float64Histogram("gen_ai.client.operation.duration")
	ttft, _ := meter.Float64Histogram("gen_ai.client.time_to_first_token")
	ttfb, _ := meter.Float64Histogram("gen_ai.server.time_to_first_byte")
	cost, _ := meter.Float64Histogram("gen_ai.client.cost.usd")
	costTotal, _ := meter.Float64Counter("gen_ai.client.cost.usd.total")
	requests, _ := meter.Int64Counter("llmtap.requests")
	misses, _ := meter.Int64Counter("llmtap.pricing.misses")

	h := &Handler{
		cfg:    config.Default(),
		tracer: tp.Tracer("test"),
		meters: telemetry.GenAIMeters{
			TokenUsage:        tokens,
			OperationDuration: dur,
			TimeToFirstToken:  ttft,
			TimeToFirstByte:   ttfb,
			CostUSD:           cost,
			CostUSDTotal:      costTotal,
		},
		requests:      requests,
		pricingMisses: misses,
		modelLabel:    labels.NewModelLabel(labels.DefaultMaxCardinality),
		pricing:       pricing.Default(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		providers:     provider.BuiltIn(),
	}
	return h, rec, tp
}

func TestObserverFinalizeUsesFallbackStatus(t *testing.T) {
	t.Parallel()
	h, rec, tp := observerHarness(t)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := h.tracer.Start(context.Background(), "test")
	info := &provider.Info{
		System:       "openai",
		Operation:    "chat",
		RequestModel: "gpt-5-mini",
		Started:      time.Now().Add(-10 * time.Millisecond),
	}
	o := newRequestObserver(h, h.providers["openai"], info, provider.ContentOpts{}, span, "openai",
		func() int { return statusClientClosedRequest })

	o.Finalize(ctx)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	got := attrInt(spans[0].Attributes(), "http.response.status_code")
	if got != statusClientClosedRequest {
		t.Fatalf("status_code = %d, want %d", got, statusClientClosedRequest)
	}
	if spans[0].Status().Code != codes.Error {
		t.Fatalf("span status = %v, want Error", spans[0].Status().Code)
	}
}

func TestObserverFinalizeDefaultsTo502(t *testing.T) {
	t.Parallel()
	h, rec, tp := observerHarness(t)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := h.tracer.Start(context.Background(), "test")
	info := &provider.Info{System: "openai", Operation: "chat", Started: time.Now()}
	o := newRequestObserver(h, h.providers["openai"], info, provider.ContentOpts{}, span, "openai", nil)

	o.Finalize(ctx)

	got := attrInt(rec.Ended()[0].Attributes(), "http.response.status_code")
	if got != http.StatusBadGateway {
		t.Fatalf("status_code = %d, want 502", got)
	}
}

func TestObserverFinalizeIdempotent(t *testing.T) {
	t.Parallel()
	h, rec, tp := observerHarness(t)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := h.tracer.Start(context.Background(), "test")
	info := &provider.Info{System: "openai", Operation: "chat", Started: time.Now()}
	o := newRequestObserver(h, h.providers["openai"], info, provider.ContentOpts{}, span, "openai",
		func() int { return 200 })

	o.Finalize(ctx)
	o.Finalize(ctx)
	o.Finalize(ctx)

	if n := len(rec.Ended()); n != 1 {
		t.Fatalf("Finalize must end the span exactly once; ended=%d", n)
	}
}

func TestObserverModifyThenFinalizeRecordsOK(t *testing.T) {
	t.Parallel()
	h, rec, tp := observerHarness(t)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := h.tracer.Start(context.Background(), "test")
	info := &provider.Info{
		System:       "openai",
		Operation:    "chat",
		RequestModel: "gpt-5-mini",
		Started:      time.Now().Add(-5 * time.Millisecond),
	}
	o := newRequestObserver(h, h.providers["openai"], info, provider.ContentOpts{}, span, "openai", nil)

	body := `{"id":"x","model":"gpt-5-mini","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	req := httptest.NewRequest(http.MethodPost, "http://example/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
	if err := o.ModifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	// Drain so parseOnClose fires Finalize on EOF.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Post-ServeHTTP Finalize must be a no-op.
	o.Finalize(context.Background())

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got := attrInt(spans[0].Attributes(), "http.response.status_code"); got != 200 {
		t.Fatalf("status_code = %d, want 200", got)
	}
	if spans[0].Status().Code != codes.Ok {
		t.Fatalf("span status = %v, want Ok", spans[0].Status().Code)
	}
	if info.OutputTokens != 1 {
		t.Fatalf("OutputTokens = %d, want 1 (parser should have run before Finalize)", info.OutputTokens)
	}
}

func attrInt(attrs []attribute.KeyValue, key string) int {
	for _, a := range attrs {
		if string(a.Key) == key {
			return int(a.Value.AsInt64())
		}
	}
	return 0
}
