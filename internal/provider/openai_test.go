package provider

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newRecorderTracer() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return rec, tp
}

func TestOpenAIParseRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"gpt-4o-mini",
		"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}],
		"temperature":0.2,
		"max_tokens":64,
		"stream":true,
		"stop":["END"]
	}`)
	rec, tp := newRecorderTracer()
	tr := tp.Tracer("t")
	_, span := tr.Start(context.Background(), "init")

	info := OpenAI{}.ParseRequest(span, "/v1/chat/completions", body, ContentOpts{})
	span.End()

	if info.Operation != "chat" {
		t.Errorf("op = %q", info.Operation)
	}
	if info.RequestModel != "gpt-4o-mini" {
		t.Errorf("model = %q", info.RequestModel)
	}
	if !info.Stream {
		t.Errorf("stream = false")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	got := spans[0]
	if got.Name() != "chat gpt-4o-mini" {
		t.Errorf("span name = %q", got.Name())
	}
	mustHaveAttr(t, got.Attributes(), "gen_ai.system", "openai")
	mustHaveAttr(t, got.Attributes(), "gen_ai.request.model", "gpt-4o-mini")
}

func TestOpenAIParseResponseJSON(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"id":"chatcmpl-1",
		"model":"gpt-4o-mini-2024-07-18",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}
	}`)
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "init")
	info := &Info{System: "openai", Operation: "chat"}

	OpenAI{}.ParseResponseJSON(span, info, body, ContentOpts{})
	span.End()

	if info.InputTokens != 10 || info.OutputTokens != 3 {
		t.Errorf("tokens = %d/%d", info.InputTokens, info.OutputTokens)
	}
	if info.ResponseModel != "gpt-4o-mini-2024-07-18" {
		t.Errorf("response model = %q", info.ResponseModel)
	}
	if len(info.FinishReasons) != 1 || info.FinishReasons[0] != "stop" {
		t.Errorf("finish reasons = %v", info.FinishReasons)
	}

	spans := rec.Ended()
	mustHaveAttrInt64(t, spans[0].Attributes(), "gen_ai.usage.input_tokens", 10)
	mustHaveAttrInt64(t, spans[0].Attributes(), "gen_ai.usage.output_tokens", 3)
}

func TestOpenAIWrapStreamFinishesOnce(t *testing.T) {
	t.Parallel()

	chunks := []map[string]any{
		{"id": "c", "model": "gpt-4o-mini", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}},
		}},
		{"id": "c", "model": "gpt-4o-mini", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"content": " world"}, "finish_reason": "stop"},
		}, "usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2}},
	}

	var b strings.Builder
	for _, c := range chunks {
		buf, _ := json.Marshal(c)
		b.WriteString("data: ")
		b.Write(buf)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")

	_, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "init")
	info := &Info{System: "openai", Operation: "chat", RequestModel: "gpt-4o-mini"}

	firstToken, done := 0, 0
	stream := OpenAI{}.WrapStream(span, info, io.NopCloser(strings.NewReader(b.String())), ContentOpts{},
		func() { firstToken++ },
		func() { done++ },
	)

	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), "[DONE]") {
		t.Errorf("forwarded body lost [DONE]")
	}
	_ = stream.Close()

	if firstToken != 1 {
		t.Errorf("firstToken called %d times", firstToken)
	}
	if done != 1 {
		t.Errorf("done called %d times (want exactly 1)", done)
	}
	if info.OutputTokens != 2 || info.InputTokens != 5 {
		t.Errorf("usage merged from final chunk: in=%d out=%d", info.InputTokens, info.OutputTokens)
	}
	if len(info.FinishReasons) != 1 || info.FinishReasons[0] != "stop" {
		t.Errorf("finish reasons = %v", info.FinishReasons)
	}
}
