package provider

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestAnthropicWrapStreamToolOnlyStreamFiresFirstToken is the A26
// regression for Anthropic. A streaming /v1/messages call where the
// model chooses to emit a tool_use block streams `content_block_delta`
// events with `delta.type = "input_json_delta"` and `partial_json` —
// never `text_delta`. The existing TTFT trigger only fires on
// `delta.Text != ""`, so tool-only streams record FirstTokenAt = zero
// and quietly undercount TTFT across the population.
//
// The fix: fire on EITHER text_delta OR input_json_delta. The
// anthropicDelta struct gains a PartialJSON field so the parser can
// observe input_json_delta payloads instead of silently dropping them.
func TestAnthropicWrapStreamToolOnlyStreamFiresFirstToken(t *testing.T) {
	t.Parallel()

	// Real Anthropic stream shape for a tool-only response:
	//   message_start
	//   content_block_start (type=tool_use)
	//   content_block_delta * N  (delta.type=input_json_delta, partial_json="...")
	//   content_block_stop
	//   message_delta (stop_reason=tool_use)
	//   message_stop
	events := []map[string]any{
		{"type": "message_start", "message": map[string]any{
			"id":    "msg_1",
			"model": "claude-3-5-sonnet-20241022",
			"usage": map[string]any{"input_tokens": 12},
		}},
		{"type": "content_block_start", "index": 0, "content_block": map[string]any{
			"type": "tool_use", "id": "toolu_1", "name": "get_weather",
		}},
		{"type": "content_block_delta", "index": 0, "delta": map[string]any{
			"type": "input_json_delta", "partial_json": `{"city":"`,
		}},
		{"type": "content_block_delta", "index": 0, "delta": map[string]any{
			"type": "input_json_delta", "partial_json": `Paris"}`,
		}},
		{"type": "content_block_stop", "index": 0},
		{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use"},
			"usage": map[string]any{"output_tokens": 7},
		},
		{"type": "message_stop"},
	}

	var b strings.Builder
	for _, ev := range events {
		buf, _ := json.Marshal(ev)
		b.WriteString("event: ")
		b.WriteString(ev["type"].(string))
		b.WriteString("\ndata: ")
		b.Write(buf)
		b.WriteString("\n\n")
	}

	_, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "init")
	info := &Info{System: "anthropic", Operation: "chat", RequestModel: "claude-3-5-sonnet-20241022"}

	done := 0
	stream := Anthropic{}.WrapStream(span, info, io.NopCloser(strings.NewReader(b.String())), ContentOpts{},
		func() { done++ },
	)
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = stream.Close()

	if done != 1 {
		t.Errorf("done called %d times, want exactly 1", done)
	}
	if info.FirstTokenAt.IsZero() {
		t.Errorf("FirstTokenAt is zero on tool-only stream; TTFT metric is silently broken")
	}
	if len(info.FinishReasons) != 1 || info.FinishReasons[0] != "tool_use" {
		t.Errorf("finish reasons = %v, want [tool_use]", info.FinishReasons)
	}
	if info.OutputTokens != 7 {
		t.Errorf("output tokens = %d, want 7", info.OutputTokens)
	}
}

func TestAnthropicThinkingDeltaFiresTTFT(t *testing.T) {
	t.Parallel()
	sse := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"considering"}}` +
		"\n\n"
	_, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "anthropic")
	info := &Info{System: "anthropic", Operation: "chat"}
	body := Anthropic{}.WrapStream(span, info, io.NopCloser(strings.NewReader(sse)), ContentOpts{}, func() {})
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	if info.FirstTokenAt.IsZero() {
		t.Fatal("thinking_delta did not set FirstTokenAt")
	}
}

func TestAnthropicCacheUsageNormalizesInputTotal(t *testing.T) {
	t.Parallel()
	info := &Info{System: "anthropic", Operation: "chat"}
	body := []byte(`{"id":"m1","model":"claude-sonnet-4","usage":{
		"input_tokens":10,"cache_read_input_tokens":20,
		"cache_creation_input_tokens":5,"output_tokens":7}}`)
	if _, ok := DecodeAnthropicResponseJSON(info, body); !ok {
		t.Fatal("decode failed")
	}
	if info.InputTokens != 35 || info.CacheReadInputTokens != 20 ||
		info.CacheCreationInputTokens != 5 || info.OutputTokens != 7 {
		t.Fatalf("usage = %+v", info)
	}
}
