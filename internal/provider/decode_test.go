package provider

import (
	"testing"
)

func TestDecodeOpenAIRequestIsPure(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"model": "gpt-5-mini",
		"temperature": 0.2,
		"max_tokens": 64,
		"messages": [{"role":"user","content":"hi"}]
	}`)
	dec := OpenAI{}.DecodeRequest("/v1/chat/completions", body)
	if !dec.OK {
		t.Fatal("decode failed")
	}
	if dec.Info.RequestModel != "gpt-5-mini" {
		t.Fatalf("model = %q", dec.Info.RequestModel)
	}
	if dec.Info.Params.Temperature == nil || *dec.Info.Params.Temperature != 0.2 {
		t.Fatalf("temperature = %v", dec.Info.Params.Temperature)
	}
	if dec.Info.Params.MaxTokens == nil || *dec.Info.Params.MaxTokens != 64 {
		t.Fatalf("max_tokens = %v", dec.Info.Params.MaxTokens)
	}
	if len(dec.Messages) != 1 {
		t.Fatalf("messages = %d", len(dec.Messages))
	}
}

func TestDecodeAnthropicRequestIsPure(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"model": "claude-fable-5",
		"max_tokens": 32,
		"top_k": 5,
		"messages": [{"role":"user","content":"hi"}]
	}`)
	dec := Anthropic{}.DecodeRequest("/v1/messages", body)
	if !dec.OK {
		t.Fatal("decode failed")
	}
	if dec.Info.RequestModel != "claude-fable-5" {
		t.Fatalf("model = %q", dec.Info.RequestModel)
	}
	if dec.Info.Params.TopK == nil || *dec.Info.Params.TopK != 5 {
		t.Fatalf("top_k = %v", dec.Info.Params.TopK)
	}
}

func TestKnownMatchesBuiltIn(t *testing.T) {
	t.Parallel()
	if !Known("openai") || !Known("anthropic") || !Known("gemini") {
		t.Fatal("BuiltIn providers must be Known")
	}
	names := KnownNames()
	if len(names) != 3 || names[0] != "anthropic" || names[1] != "gemini" || names[2] != "openai" {
		t.Fatalf("KnownNames = %v", names)
	}
}
