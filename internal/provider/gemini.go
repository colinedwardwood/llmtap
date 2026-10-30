package provider

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/colinedwardwood/llmtap/internal/genai"

	"go.opentelemetry.io/otel/trace"
)

// Gemini parses the native Google Generative Language API.
type Gemini struct{}

func (Gemini) System() string { return genai.SystemGemini }

func (Gemini) OperationFor(urlPath string) string {
	segs := pathSegments(urlPath)
	if len(segs) < 3 {
		return ""
	}
	last := segs[len(segs)-1]
	if strings.Contains(last, ":generateContent") ||
		strings.Contains(last, ":streamGenerateContent") {
		return genai.OpChat
	}
	return ""
}

type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction"`
	GenerationConfig  struct {
		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"topP"`
		TopK            *int64   `json:"topK"`
		MaxOutputTokens *int64   `json:"maxOutputTokens"`
	} `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string          `json:"text"`
	FunctionCall     json.RawMessage `json:"functionCall"`
	FunctionResponse json.RawMessage `json:"functionResponse"`
}

type geminiResponse struct {
	ResponseID   string            `json:"responseId"`
	ModelVersion string            `json:"modelVersion"`
	Candidates   []geminiCandidate `json:"candidates"`
	Usage        geminiUsage       `json:"usageMetadata"`
	Error        *struct {
		Status string `json:"status"`
	} `json:"error"`
}

type geminiCandidate struct {
	FinishReason string        `json:"finishReason"`
	Content      geminiContent `json:"content"`
}

type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
}

func geminiModelFromPath(urlPath string) string {
	for _, seg := range pathSegments(urlPath) {
		if before, ok := strings.CutSuffix(seg, ":generateContent"); ok {
			return before
		}
		if before, ok := strings.CutSuffix(seg, ":streamGenerateContent"); ok {
			return before
		}
	}
	return ""
}

func (g Gemini) ParseRequest(span trace.Span, urlPath string, body []byte, content ContentOpts) Info {
	info := Info{
		System:       genai.SystemGemini,
		Operation:    g.OperationFor(urlPath),
		RequestModel: geminiModelFromPath(urlPath),
		Started:      time.Now(),
		Stream:       strings.Contains(urlPath, ":streamGenerateContent"),
	}
	var req geminiRequest
	if json.Unmarshal(body, &req) != nil {
		emitRequestAttrs(span, &info)
		return info
	}
	info.Params.Temperature = req.GenerationConfig.Temperature
	info.Params.TopP = req.GenerationConfig.TopP
	info.Params.TopK = req.GenerationConfig.TopK
	info.Params.MaxTokens = req.GenerationConfig.MaxOutputTokens
	emitRequestAttrs(span, &info)
	if content.Capture() {
		ctx := trace.ContextWithSpan(context.Background(), span)
		if req.SystemInstruction != nil {
			content.emitRoleMessage(ctx, span, genai.EventSystemMessage, "system", geminiText(*req.SystemInstruction))
		}
		for _, msg := range req.Contents {
			event := genai.EventUserMessage
			if msg.Role == "model" {
				event = genai.EventAssistantMessage
			}
			content.emitRoleMessage(ctx, span, event, msg.Role, geminiText(msg))
		}
	}
	return info
}

func (Gemini) ParseResponseJSON(span trace.Span, info *Info, body []byte, content ContentOpts) {
	var resp geminiResponse
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	mergeGeminiResponse(info, &resp)
	emitResponseAttrs(span, info)
	if content.Capture() {
		ctx := trace.ContextWithSpan(context.Background(), span)
		for i, candidate := range resp.Candidates {
			content.emitChoice(ctx, span, i, candidate.FinishReason, geminiText(candidate.Content))
		}
	}
}

func (Gemini) WrapStream(span trace.Span, info *Info, body io.ReadCloser, content ContentOpts, onDone func()) io.ReadCloser {
	c := newStreamCollector(span, info, content, onDone)
	return c.wrap(body, func(_ string, data []byte) {
		var resp geminiResponse
		if json.Unmarshal(data, &resp) != nil {
			return
		}
		mergeGeminiResponse(info, &resp)
		for _, candidate := range resp.Candidates {
			text := geminiText(candidate.Content)
			hasTool := geminiHasFunctionCall(candidate.Content)
			if text != "" || hasTool {
				c.noteFirstToken()
				c.appendText(text)
			}
		}
	})
}

func mergeGeminiResponse(info *Info, resp *geminiResponse) {
	info.ResponseID = resp.ResponseID
	info.ResponseModel = resp.ModelVersion
	info.InputTokens = resp.Usage.PromptTokenCount
	info.OutputTokens = resp.Usage.CandidatesTokenCount
	info.CacheReadInputTokens = resp.Usage.CachedContentTokenCount
	info.ReasoningOutputTokens = resp.Usage.ThoughtsTokenCount
	if resp.Error != nil {
		info.ErrorType = resp.Error.Status
	}
	for _, candidate := range resp.Candidates {
		if candidate.FinishReason != "" {
			info.FinishReasons = append(info.FinishReasons, candidate.FinishReason)
		}
	}
}

func geminiText(content geminiContent) string {
	var out strings.Builder
	for _, part := range content.Parts {
		out.WriteString(part.Text)
	}
	return out.String()
}

func geminiHasFunctionCall(content geminiContent) bool {
	for _, part := range content.Parts {
		if len(part.FunctionCall) > 0 && string(part.FunctionCall) != "null" {
			return true
		}
	}
	return false
}

var _ Provider = (*Gemini)(nil)
