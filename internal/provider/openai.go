package provider

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/colinedwardwood/llmtap/internal/genai"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// OpenAI parses the OpenAI Chat Completions and Embeddings wire formats. By
// extension it covers every "OpenAI-compatible" provider — Azure OpenAI,
// Groq, Together, Fireworks, OpenRouter, vLLM, llama.cpp, Ollama-OpenAI mode.
type OpenAI struct{}

func (OpenAI) System() string { return genai.SystemOpenAI }

// OperationFor returns "" for paths llmtap should pass through unenriched
// (file uploads, fine-tuning, audio). v0.1 supports the two highest-volume
// endpoints; more are a small additive change.
func (OpenAI) OperationFor(urlPath string) string {
	switch {
	case strings.HasSuffix(urlPath, "/chat/completions"):
		return genai.OpChat
	case strings.HasSuffix(urlPath, "/embeddings"):
		return genai.OpEmbeddings
	default:
		return ""
	}
}

type openAIRequest struct {
	Model            string         `json:"model"`
	Messages         []openAIMessage `json:"messages"`
	Input            json.RawMessage `json:"input"` // embeddings: string or [string]
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	MaxTokens        *int64         `json:"max_tokens,omitempty"`
	MaxComplTokens   *int64         `json:"max_completion_tokens,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	Seed             *int64         `json:"seed,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	Stream           bool           `json:"stream,omitempty"`
	EncodingFormat   string         `json:"encoding_format,omitempty"`
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (OpenAI) ParseRequest(span trace.Span, urlPath string, body []byte, captureContent bool) Info {
	op := (OpenAI{}).OperationFor(urlPath)
	info := Info{
		System:    genai.SystemOpenAI,
		Operation: op,
		Started:   time.Now(),
	}
	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		span.SetAttributes(attribute.String(genai.AttrSystem, info.System))
		return info
	}

	info.RequestModel = req.Model
	info.Stream = req.Stream

	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrSystem, info.System),
		attribute.String(genai.AttrOperationName, info.Operation),
		attribute.Bool(genai.AttrStream, req.Stream),
	}
	if req.Model != "" {
		attrs = append(attrs, attribute.String(genai.AttrRequestModel, req.Model))
	}
	if req.Temperature != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestTemperature, *req.Temperature))
	}
	if req.TopP != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestTopP, *req.TopP))
	}
	switch {
	case req.MaxComplTokens != nil:
		attrs = append(attrs, attribute.Int64(genai.AttrRequestMaxTokens, *req.MaxComplTokens))
	case req.MaxTokens != nil:
		attrs = append(attrs, attribute.Int64(genai.AttrRequestMaxTokens, *req.MaxTokens))
	}
	if req.FrequencyPenalty != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestFrequencyPen, *req.FrequencyPenalty))
	}
	if req.PresencePenalty != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestPresencePen, *req.PresencePenalty))
	}
	if req.Seed != nil {
		attrs = append(attrs, attribute.Int64(genai.AttrRequestSeed, *req.Seed))
	}
	if stops := decodeStopSequences(req.Stop); len(stops) > 0 {
		attrs = append(attrs, attribute.StringSlice(genai.AttrRequestStopSequences, stops))
	}
	if req.EncodingFormat != "" && op == genai.OpEmbeddings {
		attrs = append(attrs, attribute.StringSlice(genai.AttrRequestEncodingFmts, []string{req.EncodingFormat}))
	}
	span.SetAttributes(attrs...)
	span.SetName(genai.SpanName(op, info.RequestModel))

	if captureContent && op == genai.OpChat {
		emitOpenAIRequestEvents(span, req.Messages)
	}
	return info
}

// decodeStopSequences accepts "stop" as either a string or a []string per the
// OpenAI schema. Unknown shapes are ignored rather than failing — the proxy
// must never refuse a call due to a parser quirk.
func decodeStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	return nil
}

func emitOpenAIRequestEvents(span trace.Span, msgs []openAIMessage) {
	for _, m := range msgs {
		eventName := genai.EventUserMessage
		switch m.Role {
		case "system", "developer":
			eventName = genai.EventSystemMessage
		case "assistant":
			eventName = genai.EventAssistantMessage
		case "tool":
			eventName = genai.EventToolMessage
		}
		span.AddEvent(eventName, trace.WithAttributes(
			attribute.String("role", m.Role),
			attribute.String("content", string(m.Content)),
		))
	}
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
	Data    []openAIEmbed  `json:"data"` // embeddings only
}

type openAIChoice struct {
	Index        int            `json:"index"`
	Message      openAIMessage  `json:"message"`
	Delta        openAIMessage  `json:"delta"`
	FinishReason string         `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	InputTokens      int64 `json:"input_tokens"`  // Responses API alias
	OutputTokens     int64 `json:"output_tokens"` // Responses API alias
}

type openAIEmbed struct {
	Index int `json:"index"`
}

func (OpenAI) ParseResponseJSON(span trace.Span, info *Info, body []byte, captureContent bool) {
	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	mergeOpenAIUsage(info, resp.Usage)
	if resp.Model != "" {
		info.ResponseModel = resp.Model
	}
	if resp.ID != "" {
		info.ResponseID = resp.ID
	}
	for _, c := range resp.Choices {
		if c.FinishReason != "" {
			info.FinishReasons = append(info.FinishReasons, c.FinishReason)
		}
	}

	span.SetAttributes(responseAttrs(info)...)

	if captureContent && info.Operation == genai.OpChat {
		for _, c := range resp.Choices {
			span.AddEvent(genai.EventChoice, trace.WithAttributes(
				attribute.Int("index", c.Index),
				attribute.String("finish_reason", c.FinishReason),
				attribute.String("message", string(c.Message.Content)),
			))
		}
	}
}

func mergeOpenAIUsage(info *Info, u openAIUsage) {
	if u.PromptTokens > 0 {
		info.InputTokens = u.PromptTokens
	} else if u.InputTokens > 0 {
		info.InputTokens = u.InputTokens
	}
	if u.CompletionTokens > 0 {
		info.OutputTokens = u.CompletionTokens
	} else if u.OutputTokens > 0 {
		info.OutputTokens = u.OutputTokens
	}
}

func responseAttrs(info *Info) []attribute.KeyValue {
	out := []attribute.KeyValue{}
	if info.ResponseModel != "" {
		out = append(out, attribute.String(genai.AttrResponseModel, info.ResponseModel))
	}
	if info.ResponseID != "" {
		out = append(out, attribute.String(genai.AttrResponseID, info.ResponseID))
	}
	if len(info.FinishReasons) > 0 {
		out = append(out, attribute.StringSlice(genai.AttrResponseFinishReasons, info.FinishReasons))
	}
	if info.InputTokens > 0 {
		out = append(out, attribute.Int64(genai.AttrUsageInputTokens, info.InputTokens))
	}
	if info.OutputTokens > 0 {
		out = append(out, attribute.Int64(genai.AttrUsageOutputTokens, info.OutputTokens))
	}
	return out
}

// streaming chunk: roughly the same shape as openAIResponse but `choices[].delta`
// is populated instead of `choices[].message`. The final chunk before "[DONE]"
// carries usage when the client opts in via stream_options.include_usage=true.
func (OpenAI) WrapStream(
	span trace.Span,
	info *Info,
	body io.ReadCloser,
	captureContent bool,
	onFirstToken func(),
	onDone func(),
) io.ReadCloser {
	var (
		assembled strings.Builder
		gotFirst  bool
	)

	onEvent := func(_ string, data []byte) {
		if len(data) == 0 {
			return
		}
		if string(data) == "[DONE]" {
			return
		}
		var chunk openAIResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			return
		}
		if chunk.Model != "" {
			info.ResponseModel = chunk.Model
		}
		if chunk.ID != "" {
			info.ResponseID = chunk.ID
		}
		mergeOpenAIUsage(info, chunk.Usage)
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				info.FinishReasons = append(info.FinishReasons, c.FinishReason)
			}
			if delta := string(c.Delta.Content); delta != "" {
				if !gotFirst {
					gotFirst = true
					info.FirstTokenAt = time.Now()
					onFirstToken()
				}
				if captureContent {
					assembled.WriteString(delta)
				}
			}
		}
	}

	closer := func() {
		if !info.Finished.IsZero() {
			return
		}
		info.Finished = time.Now()
		span.SetAttributes(responseAttrs(info)...)
		if captureContent && assembled.Len() > 0 {
			span.AddEvent(genai.EventChoice, trace.WithAttributes(
				attribute.String("message", assembled.String()),
			))
		}
		onDone()
	}

	return newSSETee(body, onEvent, closer)
}

var _ Provider = (*OpenAI)(nil)
