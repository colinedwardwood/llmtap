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

// Anthropic parses the Messages API ("/v1/messages"). Anthropic streams as
// named SSE events (message_start, content_block_delta, message_delta,
// message_stop) which is more structured than OpenAI; we exploit that to
// extract usage cleanly without depending on opt-in flags.
type Anthropic struct{}

func (Anthropic) System() string { return genai.SystemAnthropic }

func (Anthropic) OperationFor(urlPath string) string {
	if strings.HasSuffix(urlPath, "/messages") {
		return genai.OpChat
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

func (Anthropic) ParseRequest(span trace.Span, urlPath string, body []byte, content ContentOpts) Info {
	op := (Anthropic{}).OperationFor(urlPath)
	info := Info{
		System:    genai.SystemAnthropic,
		Operation: op,
		Started:   time.Now(),
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		span.SetAttributes(attribute.String(genai.AttrSystem, info.System))
		return info
	}

	info.RequestModel = req.Model
	info.Stream = req.Stream

	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrSystem, info.System),
		attribute.String(genai.AttrOperationName, op),
		attribute.Bool(genai.AttrStream, req.Stream),
	}
	if req.Model != "" {
		attrs = append(attrs, attribute.String(genai.AttrRequestModel, req.Model))
	}
	if req.MaxTokens != nil {
		attrs = append(attrs, attribute.Int64(genai.AttrRequestMaxTokens, *req.MaxTokens))
	}
	if req.Temperature != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestTemperature, *req.Temperature))
	}
	if req.TopP != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestTopP, *req.TopP))
	}
	if req.TopK != nil {
		attrs = append(attrs, attribute.Int64(genai.AttrRequestTopK, *req.TopK))
	}
	if len(req.StopSeq) > 0 {
		attrs = append(attrs, attribute.StringSlice(genai.AttrRequestStopSequences, req.StopSeq))
	}
	span.SetAttributes(attrs...)
	span.SetName(genai.SpanName(op, info.RequestModel))

	if content.Capture {
		if len(req.System) > 0 {
			span.AddEvent(genai.EventSystemMessage, trace.WithAttributes(
				attribute.String("content", content.Clean(string(req.System))),
			))
		}
		for _, m := range req.Messages {
			eventName := genai.EventUserMessage
			if m.Role == "assistant" {
				eventName = genai.EventAssistantMessage
			}
			span.AddEvent(eventName, trace.WithAttributes(
				attribute.String("role", m.Role),
				attribute.String("content", content.Clean(string(m.Content))),
			))
		}
	}
	return info
}

type anthropicResponse struct {
	ID         string             `json:"id"`
	Model      string             `json:"model"`
	StopReason string             `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
	Content    []anthropicContent `json:"content"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (Anthropic) ParseResponseJSON(span trace.Span, info *Info, body []byte, content ContentOpts) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
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
	info.InputTokens = resp.Usage.InputTokens
	info.OutputTokens = resp.Usage.OutputTokens

	span.SetAttributes(responseAttrs(info)...)

	if content.Capture {
		var b strings.Builder
		for _, c := range resp.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		if b.Len() > 0 {
			span.AddEvent(genai.EventChoice, trace.WithAttributes(
				attribute.String("message", content.Clean(b.String())),
			))
		}
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
	Type       string `json:"type,omitempty"`
	Text       string `json:"text,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
}

func (Anthropic) WrapStream(
	span trace.Span,
	info *Info,
	body io.ReadCloser,
	content ContentOpts,
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
				if ev.Message.Usage.InputTokens > 0 {
					info.InputTokens = ev.Message.Usage.InputTokens
				}
			}
		case "content_block_delta":
			if ev.Delta != nil && ev.Delta.Text != "" {
				if !gotFirst {
					gotFirst = true
					info.FirstTokenAt = time.Now()
					onFirstToken()
				}
				if content.Capture {
					assembled.WriteString(ev.Delta.Text)
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				info.FinishReasons = append(info.FinishReasons, ev.Delta.StopReason)
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				info.OutputTokens = ev.Usage.OutputTokens
			}
		}
	}

	closer := func() {
		if !info.Finished.IsZero() {
			return
		}
		info.Finished = time.Now()
		span.SetAttributes(responseAttrs(info)...)
		if content.Capture && assembled.Len() > 0 {
			span.AddEvent(genai.EventChoice, trace.WithAttributes(
				attribute.String("message", content.Clean(assembled.String())),
			))
		}
		onDone()
	}

	overflow := func() { span.AddEvent("llmtap.sse_parser_overflow") }
	return newSSETee(body, onEvent, overflow, closer)
}

var _ Provider = (*Anthropic)(nil)
