package provider

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/colinedwardwood/llmtap/internal/genai"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Anthropic parses the Messages API ("/v1/messages"). Anthropic streams as
// named SSE events (message_start, content_block_delta, message_delta,
// message_stop) which is more structured than OpenAI; we exploit that to
// extract usage cleanly without depending on opt-in flags.
type Anthropic struct{}

func (Anthropic) System() string { return genai.SystemAnthropic }

// OperationFor recognizes the Anthropic Messages API. Match is
// strict-by-segment to avoid suffix collisions: /v1/anthropic/.well-known/messages
// is NOT a Messages call even though it ends in `/messages`. The
// accepted shape is `(prefix...)/v\d+/messages`.
func (Anthropic) OperationFor(urlPath string) string {
	segs := pathSegments(urlPath)
	if n := len(segs); n >= 2 && segs[n-1] == "messages" {
		if isAPIParent(segs[:n-1]) {
			return genai.OpChat
		}
	}
	return ""
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      json.RawMessage    `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   *int64             `json:"max_tokens,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	TopK        *int64             `json:"top_k,omitempty"`
	StopSeq     []string           `json:"stop_sequences,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// decodedAnthropicRequest is the pure-decode result for the Messages API.
type decodedAnthropicRequest struct {
	Info     Info
	System   json.RawMessage
	Messages []anthropicMessage
	OK       bool
}

// DecodeRequest parses a Messages body without touching a span.
func (a Anthropic) DecodeRequest(urlPath string, body []byte) decodedAnthropicRequest {
	info := Info{
		System:    genai.SystemAnthropic,
		Operation: a.OperationFor(urlPath),
		Started:   time.Now(),
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return decodedAnthropicRequest{Info: info, OK: false}
	}
	info.RequestModel = req.Model
	info.Stream = req.Stream
	info.Params = RequestParams{
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		MaxTokens:     req.MaxTokens,
		StopSequences: req.StopSeq,
	}
	return decodedAnthropicRequest{
		Info:     info,
		System:   req.System,
		Messages: req.Messages,
		OK:       true,
	}
}

func (a Anthropic) ParseRequest(span trace.Span, urlPath string, body []byte, content ContentOpts) Info {
	dec := a.DecodeRequest(urlPath, body)
	if !dec.OK {
		span.SetAttributes(attribute.String(genai.AttrSystem, dec.Info.System))
		return dec.Info
	}
	emitRequestAttrs(span, &dec.Info)
	if content.Capture() {
		ctx := trace.ContextWithSpan(context.Background(), span)
		if len(dec.System) > 0 {
			content.emitRoleMessage(ctx, span, genai.EventSystemMessage, "system", string(dec.System))
		}
		for _, m := range dec.Messages {
			eventName := genai.EventUserMessage
			if m.Role == "assistant" {
				eventName = genai.EventAssistantMessage
			}
			content.emitRoleMessage(ctx, span, eventName, m.Role, string(m.Content))
		}
	}
	return dec.Info
}

type anthropicResponse struct {
	ID         string             `json:"id"`
	Model      string             `json:"model"`
	StopReason string             `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
	Content    []anthropicContent `json:"content"`
	Error      *anthropicError    `json:"error"`
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type anthropicError struct {
	Type string `json:"type"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// DecodeAnthropicResponseJSON merges response fields into info. Returns
// assembled text for optional content emit.
func DecodeAnthropicResponseJSON(info *Info, body []byte) (text string, ok bool) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", false
	}
	if resp.Model != "" {
		info.ResponseModel = resp.Model
	}
	if resp.ID != "" {
		info.ResponseID = resp.ID
	}
	if resp.StopReason != "" {
		info.FinishReasons = append(info.FinishReasons, resp.StopReason)
	}
	mergeAnthropicUsage(info, resp.Usage)
	if resp.Error != nil {
		info.ErrorType = resp.Error.Type
	}
	var b strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), true
}

func mergeAnthropicUsage(info *Info, usage anthropicUsage) {
	if usage.InputTokens > 0 || usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0 {
		info.InputTokens = usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		info.CacheReadInputTokens = usage.CacheReadInputTokens
		info.CacheCreationInputTokens = usage.CacheCreationInputTokens
	}
	if usage.OutputTokens > 0 {
		info.OutputTokens = usage.OutputTokens
	}
}

func (Anthropic) ParseResponseJSON(span trace.Span, info *Info, body []byte, content ContentOpts) {
	text, ok := DecodeAnthropicResponseJSON(info, body)
	if !ok {
		return
	}
	emitResponseAttrs(span, info)
	if content.Capture() && text != "" {
		ctx := trace.ContextWithSpan(context.Background(), span)
		content.emitChoice(ctx, span, 0, "", text)
	}
}

// Streaming events on /v1/messages:
//   - message_start: {type, message:{id, model, usage:{input_tokens}}}
//   - content_block_delta: {delta:{type:"text_delta", text}}
//   - message_delta: {delta:{stop_reason}, usage:{output_tokens}}
//   - message_stop
type anthropicStreamEvent struct {
	Type    string             `json:"type"`
	Message *anthropicResponse `json:"message,omitempty"`
	Delta   *anthropicDelta    `json:"delta,omitempty"`
	Usage   *anthropicUsage    `json:"usage,omitempty"`
}

type anthropicDelta struct {
	Type     string `json:"type,omitempty"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	// PartialJSON carries one chunk of a streaming tool_use block's
	// `input` JSON. Anthropic emits these via content_block_delta with
	// type=input_json_delta when the model is calling a tool. We don't
	// reassemble the arguments — only the *presence* of a delta is
	// needed to fire TTFT and prove the stream is producing output.
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

func (Anthropic) WrapStream(
	span trace.Span,
	info *Info,
	body io.ReadCloser,
	content ContentOpts,
	onDone func(),
) io.ReadCloser {
	c := newStreamCollector(span, info, content, onDone)
	return c.wrap(body, func(_ string, data []byte) {
		if len(data) == 0 {
			return
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				if ev.Message.Model != "" {
					info.ResponseModel = ev.Message.Model
				}
				if ev.Message.ID != "" {
					info.ResponseID = ev.Message.ID
				}
				mergeAnthropicUsage(info, ev.Message.Usage)
			}
		case "content_block_delta":
			// Anthropic content_block_delta carries either a text_delta
			// (model is producing a text block) or an input_json_delta
			// (model is producing a tool_use block's arguments JSON).
			// Both shapes prove the upstream is generating tokens, so
			// both trigger TTFT — without this, every tool-only stream
			// records FirstTokenAt=0.
			if ev.Delta != nil {
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" {
						c.noteFirstToken()
						c.appendText(ev.Delta.Text)
					}
				case "input_json_delta":
					c.noteFirstToken()
					// Tool-argument JSON fragments are intentionally not
					// assembled — they belong to a tool_use block, not
					// to assistant text, and concatenating them onto the
					// transcript would produce garbage.
				case "thinking_delta":
					if ev.Delta.Thinking != "" {
						c.noteFirstToken()
					}
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				info.FinishReasons = append(info.FinishReasons, ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				mergeAnthropicUsage(info, *ev.Usage)
			}
		}
	})
}

var _ Provider = (*Anthropic)(nil)
