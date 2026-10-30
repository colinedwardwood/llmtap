package provider

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
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
//
// Match is strict-by-segment, not suffix: /v1/files/chat/completions is a
// real OpenAI sub-resource of the Files API, not a chat call, so it must
// NOT be enriched as one. The accepted shapes:
//
//	(prefix...)/v\d+/chat/completions               — OpenAI / OpenAI-compat
//	(prefix...)/v\d+/embeddings                     — OpenAI / OpenAI-compat
//	(prefix...)/deployments/{name}/chat/completions — Azure OpenAI
//	(prefix...)/deployments/{name}/embeddings       — Azure OpenAI
//
// `prefix` is any number of pass-through segments (tenant, region, etc.).
func (OpenAI) OperationFor(urlPath string) string {
	segs := pathSegments(urlPath)
	// chat/completions: last two segments must be ["chat", "completions"].
	if n := len(segs); n >= 3 && segs[n-1] == "completions" && segs[n-2] == "chat" {
		if isAPIParent(segs[:n-2]) {
			return genai.OpChat
		}
	}
	// embeddings: last segment must be "embeddings".
	if n := len(segs); n >= 2 && segs[n-1] == "embeddings" {
		if isAPIParent(segs[:n-1]) {
			return genai.OpEmbeddings
		}
	}
	// Responses API: /v1/responses and Azure-compatible deployments.
	if n := len(segs); n >= 2 && segs[n-1] == "responses" {
		if isAPIParent(segs[:n-1]) {
			return genai.OpResponses
		}
	}
	return ""
}

type openAIRequest struct {
	Model            string          `json:"model"`
	Messages         []openAIMessage `json:"messages"`
	Input            json.RawMessage `json:"input"` // embeddings: string or [string]
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	MaxTokens        *int64          `json:"max_tokens,omitempty"`
	MaxComplTokens   *int64          `json:"max_completion_tokens,omitempty"`
	MaxOutputTokens  *int64          `json:"max_output_tokens,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	EncodingFormat   string          `json:"encoding_format,omitempty"`
	Instructions     string          `json:"instructions,omitempty"`
	ServiceTier      string          `json:"service_tier,omitempty"`
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// ToolCalls is populated on streaming deltas when the model emits
	// tool calls instead of (or alongside) text. Held as raw JSON so
	// the proxy doesn't need to model the per-chunk argument fragment
	// schema; presence is all the streaming TTFT path needs.
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
	Refusal   string          `json:"refusal,omitempty"`
}

// decodedOpenAIRequest is the pure-decode result: Info + messages for
// optional content emission. Span-free so record/replay can capture it.
type decodedOpenAIRequest struct {
	Info     Info
	Messages []openAIMessage
	OK       bool // false when JSON unmarshal failed
}

// DecodeRequest parses a Chat Completions / Embeddings body without
// touching a span. Pure decode half of ParseRequest.
func (o OpenAI) DecodeRequest(urlPath string, body []byte) decodedOpenAIRequest {
	info := Info{
		System:    genai.SystemOpenAI,
		Operation: o.OperationFor(urlPath),
		Started:   time.Now(),
	}
	switch {
	case isOpenAIResponsesPath(urlPath):
		info.API = APIResponses
	case info.Operation == genai.OpEmbeddings:
		info.API = APIEmbeddings
	default:
		info.API = APIChatCompletions
	}
	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return decodedOpenAIRequest{Info: info, OK: false}
	}
	info.RequestModel = req.Model
	info.Stream = req.Stream
	info.ServiceTier = req.ServiceTier
	info.Params = RequestParams{
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		Seed:             req.Seed,
		StopSequences:    decodeStopSequences(req.Stop),
	}
	switch {
	case req.MaxOutputTokens != nil:
		info.Params.MaxTokens = req.MaxOutputTokens
	case req.MaxComplTokens != nil:
		info.Params.MaxTokens = req.MaxComplTokens
	case req.MaxTokens != nil:
		info.Params.MaxTokens = req.MaxTokens
	}
	if req.EncodingFormat != "" && info.Operation == genai.OpEmbeddings {
		info.Params.EncodingFormats = []string{req.EncodingFormat}
	}
	messages := req.Messages
	if info.API == APIResponses {
		messages = decodeResponsesInput(req.Input)
		if req.Instructions != "" {
			messages = append([]openAIMessage{{
				Role: "system", Content: json.RawMessage(strconv.Quote(req.Instructions)),
			}}, messages...)
		}
	}
	return decodedOpenAIRequest{Info: info, Messages: messages, OK: true}
}

func (o OpenAI) ParseRequest(span trace.Span, urlPath string, body []byte, content ContentOpts) Info {
	dec := o.DecodeRequest(urlPath, body)
	if !dec.OK {
		span.SetAttributes(attribute.String(genai.AttrSystem, dec.Info.System))
		return dec.Info
	}
	emitRequestAttrs(span, &dec.Info)
	if content.Capture() && dec.Info.Operation == genai.OpChat {
		ctx := trace.ContextWithSpan(context.Background(), span)
		emitOpenAIRequestEvents(ctx, span, dec.Messages, content)
	}
	return dec.Info
}

func isOpenAIResponsesPath(urlPath string) bool {
	segs := pathSegments(urlPath)
	if len(segs) < 2 || segs[len(segs)-1] != "responses" {
		return false
	}
	return isAPIParent(segs[:len(segs)-1])
}

func decodeResponsesInput(raw json.RawMessage) []openAIMessage {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		content, _ := json.Marshal(text)
		return []openAIMessage{{Role: "user", Content: content}}
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		messages := make([]openAIMessage, 0, len(items))
		for _, item := range items {
			var typed struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(item, &typed) != nil ||
				(typed.Type != "" && typed.Type != "message") {
				continue
			}
			text := decodeOpenAIContent(typed.Content, "input_text", "text")
			if text == "" {
				continue
			}
			encoded, _ := json.Marshal(text)
			messages = append(messages, openAIMessage{Role: typed.Role, Content: encoded})
		}
		return messages
	}
	return nil
}

func decodeOpenAIContent(raw json.RawMessage, acceptedTypes ...string) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	allowed := make(map[string]struct{}, len(acceptedTypes))
	for _, typ := range acceptedTypes {
		allowed[typ] = struct{}{}
	}
	var out strings.Builder
	for _, part := range parts {
		if _, ok := allowed[part.Type]; ok {
			out.WriteString(part.Text)
		}
	}
	return out.String()
}

// hasToolCalls reports whether a streaming delta's `tool_calls` field
// represents a non-empty tool-call event. Absent, null, and empty-array
// shapes all count as "no tool call here" — only a populated array fires.
func hasToolCalls(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "[]":
		return false
	}
	return true
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

func emitOpenAIRequestEvents(ctx context.Context, span trace.Span, msgs []openAIMessage, content ContentOpts) {
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
		content.emitRoleMessage(ctx, span, eventName, m.Role,
			decodeOpenAIContent(m.Content, "text", "input_text", "output_text"))
	}
}

type openAIResponse struct {
	ID                string                 `json:"id"`
	Model             string                 `json:"model"`
	Choices           []openAIChoice         `json:"choices"`
	Usage             openAIUsage            `json:"usage"`
	Data              []openAIEmbed          `json:"data"` // embeddings only
	Status            string                 `json:"status"`
	ServiceTier       string                 `json:"service_tier"`
	Output            []openAIResponseOutput `json:"output"`
	Error             *openAIResponseError   `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	Delta        openAIMessage `json:"delta"`
	FinishReason string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens       int64 `json:"prompt_tokens"`
	CompletionTokens   int64 `json:"completion_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokens        int64 `json:"input_tokens"`  // Responses API alias
	OutputTokens       int64 `json:"output_tokens"` // Responses API alias
	InputTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type openAIResponseOutput struct {
	Type      string                  `json:"type"`
	Role      string                  `json:"role"`
	Name      string                  `json:"name"`
	Arguments string                  `json:"arguments"`
	Content   []openAIResponseContent `json:"content"`
}

type openAIResponseContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type openAIResponseError struct {
	Type string `json:"type"`
	Code string `json:"code"`
}

type openAIEmbed struct {
	Index int `json:"index"`
}

// DecodeResponseJSON merges response fields into info from a complete
// non-streaming JSON body. Returns the choices for optional content emit.
func DecodeOpenAIResponseJSON(info *Info, body []byte) (choices []openAIChoice, ok bool) {
	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false
	}
	mergeOpenAIUsage(info, resp.Usage)
	if resp.Model != "" {
		info.ResponseModel = resp.Model
	}
	if resp.ID != "" {
		info.ResponseID = resp.ID
	}
	info.ServiceTier = resp.ServiceTier
	if resp.Error != nil {
		info.ErrorType = resp.Error.Code
		if info.ErrorType == "" {
			info.ErrorType = resp.Error.Type
		}
	}
	if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason != "" {
		info.FinishReasons = append(info.FinishReasons, resp.IncompleteDetails.Reason)
	}
	for _, output := range resp.Output {
		for _, part := range output.Content {
			if part.Refusal != "" && info.Refusal == "" {
				info.Refusal = "true"
			}
		}
	}
	for _, c := range resp.Choices {
		if c.FinishReason != "" {
			info.FinishReasons = append(info.FinishReasons, c.FinishReason)
		}
		if c.Message.Refusal != "" || c.Delta.Refusal != "" {
			info.Refusal = "true"
		}
	}
	if len(resp.Output) > 0 {
		choices = append(choices, responsesOutputChoices(resp.Output)...)
	}
	return choices, true
}

func (OpenAI) ParseResponseJSON(span trace.Span, info *Info, body []byte, content ContentOpts) {
	choices, ok := DecodeOpenAIResponseJSON(info, body)
	if !ok {
		return
	}
	emitResponseAttrs(span, info)
	if content.Capture() && info.Operation == genai.OpChat {
		ctx := trace.ContextWithSpan(context.Background(), span)
		for _, c := range choices {
			content.emitChoice(ctx, span, c.Index, c.FinishReason,
				decodeOpenAIContent(c.Message.Content, "text", "output_text"))
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
	info.CacheReadInputTokens = u.InputTokensDetails.CachedTokens
	info.CacheCreationInputTokens = u.InputTokensDetails.CacheWriteTokens
	info.ReasoningOutputTokens = u.OutputTokensDetails.ReasoningTokens
}

func responsesOutputChoices(output []openAIResponseOutput) []openAIChoice {
	choices := make([]openAIChoice, 0, len(output))
	for i, item := range output {
		if item.Type != "message" {
			continue
		}
		var text strings.Builder
		var refusal strings.Builder
		for _, part := range item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				text.WriteString(part.Text)
			}
			if part.Type == "refusal" {
				refusal.WriteString(part.Refusal)
			}
		}
		message := text.String()
		if message == "" {
			message = refusal.String()
		}
		raw, _ := json.Marshal(message)
		choices = append(choices, openAIChoice{Index: i, Message: openAIMessage{
			Role: item.Role, Content: raw,
		}})
	}
	return choices
}

// streaming chunk: roughly the same shape as openAIResponse but `choices[].delta`
// is populated instead of `choices[].message`. The final chunk before "[DONE]"
// carries usage when the client opts in via stream_options.include_usage=true.
func (OpenAI) WrapStream(
	span trace.Span,
	info *Info,
	body io.ReadCloser,
	content ContentOpts,
	onDone func(),
) io.ReadCloser {
	c := newStreamCollector(span, info, content, onDone)
	return c.wrap(body, func(event string, data []byte) {
		if len(data) == 0 || string(data) == "[DONE]" {
			return
		}
		if info.API == APIResponses {
			var evt struct {
				Type     string         `json:"type"`
				Delta    string         `json:"delta"`
				Code     string         `json:"code"`
				Response openAIResponse `json:"response"`
			}
			if json.Unmarshal(data, &evt) != nil {
				return
			}
			kind := evt.Type
			if kind == "" {
				kind = event
			}
			switch kind {
			case "response.output_text.delta":
				if evt.Delta != "" {
					c.noteFirstToken()
					c.appendText(evt.Delta)
				}
			case "response.refusal.delta":
				if evt.Delta != "" {
					info.Refusal = "true"
					c.noteFirstToken()
					c.appendText(evt.Delta)
				}
			case "response.completed", "response.failed", "response.incomplete":
				mergeResponsesEvent(info, evt.Response)
			case "error":
				info.ErrorType = evt.Code
			}
			if responsesGeneratedDelta(kind) && evt.Delta != "" {
				c.noteFirstToken()
			}
			if responsesToolEvent(kind) {
				if span.IsRecording() {
					span.AddEvent(genai.EventToolCall,
						trace.WithAttributes(attribute.String("type", kind)))
				}
			}
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
		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				info.FinishReasons = append(info.FinishReasons, ch.FinishReason)
			}
			delta := string(ch.Delta.Content)
			// A tool-only stream populates delta.tool_calls and leaves
			// delta.content empty. Both shapes count as a "first token"
			// for TTFT — without this, tool-using calls record
			// FirstTokenAt=0 and skew every operator's TTFT histogram.
			if delta != "" || hasToolCalls(ch.Delta.ToolCalls) {
				c.noteFirstToken()
				// Only text content is assembled — tool-call arguments
				// stream as opaque JSON fragments and aren't meaningful
				// as concatenated text on the trace.
				c.appendText(delta)
			}
		}
	})
}

func responsesToolEvent(kind string) bool {
	if strings.HasPrefix(kind, "response.function_call_arguments.") ||
		strings.HasPrefix(kind, "response.custom_tool_call_input.") ||
		strings.HasPrefix(kind, "response.mcp_call_arguments.") ||
		strings.HasPrefix(kind, "response.code_interpreter_call_code.") {
		return true
	}
	for _, marker := range []string{
		".web_search_call.", ".file_search_call.", ".code_interpreter_call.",
		".computer_call.", ".mcp_call.", ".image_generation_call.",
	} {
		if strings.Contains(kind, marker) {
			return true
		}
	}
	return false
}

func responsesGeneratedDelta(kind string) bool {
	switch kind {
	case "response.output_text.delta", "response.refusal.delta",
		"response.reasoning_text.delta", "response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
		"response.mcp_call_arguments.delta", "response.code_interpreter_call_code.delta":
		return true
	}
	return false
}

func mergeResponsesEvent(info *Info, resp openAIResponse) {
	if resp.ID != "" {
		info.ResponseID = resp.ID
	}
	if resp.Model != "" {
		info.ResponseModel = resp.Model
	}
	mergeOpenAIUsage(info, resp.Usage)
	info.ServiceTier = resp.ServiceTier
	if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason != "" {
		info.FinishReasons = append(info.FinishReasons, resp.IncompleteDetails.Reason)
	}
	if resp.Error != nil {
		info.ErrorType = resp.Error.Code
		if info.ErrorType == "" {
			info.ErrorType = resp.Error.Type
		}
	}
}

var _ Provider = (*OpenAI)(nil)
