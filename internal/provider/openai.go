package provider

import (
	"encoding/json"
	"io"
	"path"
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
	return ""
}

// pathSegments returns the cleaned, non-empty `/`-separated segments of
// urlPath. A trailing slash returns nil — operation endpoints are exact
// resources, not directories, so `/v1/chat/completions/` is rejected.
// An empty urlPath returns a nil slice.
func pathSegments(urlPath string) []string {
	if urlPath == "" || urlPath == "/" {
		return nil
	}
	// Trailing slash means "this is a directory/collection", not the
	// operation itself. Reject up front rather than let path.Clean
	// silently rewrite it away.
	if strings.HasSuffix(urlPath, "/") {
		return nil
	}
	cleaned := path.Clean(urlPath)
	if cleaned == "/" || cleaned == "." {
		return nil
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		return nil
	}
	return strings.Split(cleaned, "/")
}

// isAPIParent reports whether the segments immediately preceding an
// operation segment look like a real API namespace boundary. The
// segment immediately before the operation must be either a version
// segment (`v\d+`, case-insensitive) or, for the Azure OpenAI deployment
// shape, follow a `deployments/{name}` pair. Any other parent (e.g.
// `files`, `.well-known`) is treated as a sub-resource collision and
// the operation is rejected.
func isAPIParent(parents []string) bool {
	if len(parents) == 0 {
		return false
	}
	last := parents[len(parents)-1]
	if isVersionSegment(last) {
		return true
	}
	// Azure shape: .../deployments/{anything}/<op>
	if len(parents) >= 2 && parents[len(parents)-2] == "deployments" {
		return true
	}
	return false
}

// isVersionSegment reports whether s looks like `vN` for some
// positive-integer N (case-insensitive). Matches `v1`, `V2`, `v10`;
// rejects `version`, `v`, `v1beta`, `1`.
func isVersionSegment(s string) bool {
	if len(s) < 2 {
		return false
	}
	if s[0] != 'v' && s[0] != 'V' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

type openAIRequest struct {
	Model            string          `json:"model"`
	Messages         []openAIMessage `json:"messages"`
	Input            json.RawMessage `json:"input"` // embeddings: string or [string]
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	MaxTokens        *int64          `json:"max_tokens,omitempty"`
	MaxComplTokens   *int64          `json:"max_completion_tokens,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	EncodingFormat   string          `json:"encoding_format,omitempty"`
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// ToolCalls is populated on streaming deltas when the model emits
	// tool calls instead of (or alongside) text. Held as raw JSON so
	// the proxy doesn't need to model the per-chunk argument fragment
	// schema; presence is all the streaming TTFT path needs.
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

func (OpenAI) ParseRequest(span trace.Span, urlPath string, body []byte, content ContentOpts) Info {
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

	if content.Capture && op == genai.OpChat {
		emitOpenAIRequestEvents(span, req.Messages, content)
	}
	return info
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

func emitOpenAIRequestEvents(span trace.Span, msgs []openAIMessage, content ContentOpts) {
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
			attribute.String("content", content.Clean(string(m.Content))),
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
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	Delta        openAIMessage `json:"delta"`
	FinishReason string        `json:"finish_reason"`
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

func (OpenAI) ParseResponseJSON(span trace.Span, info *Info, body []byte, content ContentOpts) {
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

	if content.Capture && info.Operation == genai.OpChat {
		for _, c := range resp.Choices {
			span.AddEvent(genai.EventChoice, trace.WithAttributes(
				attribute.Int("index", c.Index),
				attribute.String("finish_reason", c.FinishReason),
				attribute.String("message", content.Clean(string(c.Message.Content))),
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
			delta := string(c.Delta.Content)
			// A tool-only stream populates delta.tool_calls and leaves
			// delta.content empty. Both shapes count as a "first token"
			// for TTFT — without this, tool-using calls record
			// FirstTokenAt=0 and skew every operator's TTFT histogram.
			if delta != "" || hasToolCalls(c.Delta.ToolCalls) {
				if !gotFirst {
					gotFirst = true
					info.FirstTokenAt = time.Now()
					onFirstToken()
				}
				// Only text content is assembled — tool-call arguments
				// stream as opaque JSON fragments and aren't meaningful
				// as concatenated text on the trace.
				if content.Capture && delta != "" {
					assembled.WriteString(delta)
				}
			}
		}
	}

	var tee *sseTee
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
		if n := tee.Overflows(); n > 0 {
			span.SetAttributes(attribute.Int("llmtap.sse_parser_overflows", n))
		}
		onDone()
	}

	tee = newSSETee(body, onEvent, nil, closer)
	return tee
}

var _ Provider = (*OpenAI)(nil)
