package provider

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestOpenAIResponsesRequestAndResponse(t *testing.T) {
	t.Parallel()
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("test").Start(context.Background(), "responses")

	request := []byte(`{
		"model":"gpt-5-mini",
		"instructions":"be concise",
		"input":"hello",
		"max_output_tokens":128,
		"stream":false
	}`)
	info := OpenAI{}.ParseRequest(span, "/v1/responses", request, ContentOpts{})
	if info.Operation != "chat" || info.API != APIResponses || info.RequestModel != "gpt-5-mini" {
		t.Fatalf("request info = %+v", info)
	}
	if info.Params.MaxTokens == nil || *info.Params.MaxTokens != 128 {
		t.Fatalf("max output tokens = %+v", info.Params.MaxTokens)
	}

	response := []byte(`{
		"id":"resp_1",
		"model":"gpt-5-mini-2026-06-01",
		"status":"completed",
		"service_tier":"priority",
		"output":[{"type":"message","role":"assistant","content":[
			{"type":"output_text","text":"hello back"}
		]}],
		"usage":{
			"input_tokens":20,
			"output_tokens":7,
			"input_tokens_details":{"cached_tokens":12},
			"output_tokens_details":{"reasoning_tokens":3}
		}
	}`)
	OpenAI{}.ParseResponseJSON(span, &info, response, ContentOpts{})
	span.End()

	if info.ResponseID != "resp_1" || info.ResponseModel == "" {
		t.Fatalf("response identity = %+v", info)
	}
	if info.InputTokens != 20 || info.OutputTokens != 7 ||
		info.CacheReadInputTokens != 12 || info.ReasoningOutputTokens != 3 {
		t.Fatalf("usage = %+v", info)
	}
	if info.ServiceTier != "priority" {
		t.Fatalf("service tier = %q", info.ServiceTier)
	}
	if len(info.FinishReasons) != 0 {
		t.Fatalf("Responses status incorrectly emitted as finish reason: %v", info.FinishReasons)
	}
	attrs := rec.Ended()[0].Attributes()
	mustHaveAttrInt64(t, attrs, "gen_ai.usage.cache_read.input_tokens", 12)
	mustHaveAttrInt64(t, attrs, "gen_ai.usage.reasoning.output_tokens", 3)
}

func TestOpenAIResponsesStructuralEventsDoNotFireTTFT(t *testing.T) {
	t.Parallel()
	sse := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\"}}\n\n" +
		"data: {\"type\":\"response.web_search_call.in_progress\"}\n\n"
	_, tp := newRecorderTracer()
	_, span := tp.Tracer("test").Start(context.Background(), "responses")
	info := &Info{System: "openai", Operation: "chat", API: APIResponses}
	stream := OpenAI{}.WrapStream(span, info, io.NopCloser(strings.NewReader(sse)), ContentOpts{}, func() {})
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if !info.FirstTokenAt.IsZero() {
		t.Fatal("structural/tool lifecycle event incorrectly counted as first token")
	}
}

func TestOpenAIResponsesStreamingTextAndTool(t *testing.T) {
	t.Parallel()
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_2","model":"gpt-5-mini"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"city\":\""}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_2","model":"gpt-5-mini","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1}}}}`,
		``,
	}, "\n")

	_, tp := newRecorderTracer()
	_, span := tp.Tracer("test").Start(context.Background(), "responses")
	info := &Info{System: "openai", Operation: "chat", API: APIResponses, RequestModel: "gpt-5-mini"}
	done := 0
	body := OpenAI{}.WrapStream(span, info, io.NopCloser(strings.NewReader(sse)), ContentOpts{}, func() { done++ })
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	_ = body.Close()

	if info.FirstTokenAt.IsZero() {
		t.Fatal("tool/text Responses event did not set TTFT")
	}
	if info.InputTokens != 5 || info.OutputTokens != 2 || info.ReasoningOutputTokens != 1 {
		t.Fatalf("stream usage = %+v", info)
	}
	if done != 1 {
		t.Fatalf("done = %d", done)
	}
}

func TestOpenAIResponsesFixtureCorpus(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("testdata/openai-responses-completed.json")
	if err != nil {
		t.Fatal(err)
	}
	info := &Info{System: "openai", Operation: "chat", API: APIResponses}
	_, tp := newRecorderTracer()
	_, span := tp.Tracer("test").Start(context.Background(), "fixture")
	OpenAI{}.ParseResponseJSON(span, info, body, ContentOpts{})
	if info.CacheReadInputTokens != 12 || info.ReasoningOutputTokens != 3 || info.ServiceTier != "default" {
		t.Fatalf("fixture parse = %+v", info)
	}
}
