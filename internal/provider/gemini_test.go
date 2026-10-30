package provider

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestGeminiNativeParse(t *testing.T) {
	t.Parallel()
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("test").Start(context.Background(), "gemini")
	body := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hello"}]}],
		"generationConfig":{"temperature":0.2,"maxOutputTokens":64}
	}`)
	info := Gemini{}.ParseRequest(span, "/v1beta/models/gemini-2.5-pro:generateContent", body, ContentOpts{})
	if info.System != "gcp.gen_ai" || info.RequestModel != "gemini-2.5-pro" || info.Operation != "chat" {
		t.Fatalf("request = %+v", info)
	}
	response := []byte(`{
		"responseId":"r1","modelVersion":"gemini-2.5-pro-001",
		"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"hi"}]}}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"cachedContentTokenCount":3,"thoughtsTokenCount":2}
	}`)
	Gemini{}.ParseResponseJSON(span, &info, response, ContentOpts{})
	span.End()
	if info.InputTokens != 10 || info.OutputTokens != 4 || info.CacheReadInputTokens != 3 || info.ReasoningOutputTokens != 2 {
		t.Fatalf("usage = %+v", info)
	}
	mustHaveAttr(t, rec.Ended()[0].Attributes(), "gen_ai.provider.name", "gcp.gen_ai")
}

func TestGeminiNativeStreamToolFiresTTFT(t *testing.T) {
	t.Parallel()
	sse := `data: {"responseId":"r1","modelVersion":"gemini-2.5-pro","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}}}]}}]}` + "\n\n"
	_, tp := newRecorderTracer()
	_, span := tp.Tracer("test").Start(context.Background(), "gemini")
	info := &Info{System: "gemini", Operation: "chat"}
	stream := Gemini{}.WrapStream(span, info, io.NopCloser(strings.NewReader(sse)), ContentOpts{}, func() {})
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if info.FirstTokenAt.IsZero() {
		t.Fatal("functionCall stream did not set TTFT")
	}
}
