// Package genai centralises OpenTelemetry GenAI semantic-convention keys and
// values used by llmtap.
//
// We define our own constants rather than importing them from a shipping
// semconv version because (a) the GenAI namespace has been actively
// stabilising through 2024-2025, (b) llmtap should compile against many
// otel-go releases, and (c) the tight surface keeps a clean upgrade story.
//
// Spec: https://opentelemetry.io/docs/specs/semconv/gen-ai/
package genai

// Span attribute keys (semconv GenAI).
const (
	AttrSystem                = "gen_ai.system"
	AttrOperationName         = "gen_ai.operation.name"
	AttrRequestModel          = "gen_ai.request.model"
	AttrRequestTemperature    = "gen_ai.request.temperature"
	AttrRequestTopP           = "gen_ai.request.top_p"
	AttrRequestTopK           = "gen_ai.request.top_k"
	AttrRequestMaxTokens      = "gen_ai.request.max_tokens"
	AttrRequestFrequencyPen   = "gen_ai.request.frequency_penalty"
	AttrRequestPresencePen    = "gen_ai.request.presence_penalty"
	AttrRequestSeed           = "gen_ai.request.seed"
	AttrRequestStopSequences  = "gen_ai.request.stop_sequences"
	AttrRequestEncodingFmts   = "gen_ai.request.encoding_formats"
	AttrResponseID            = "gen_ai.response.id"
	AttrResponseModel         = "gen_ai.response.model"
	AttrResponseFinishReasons = "gen_ai.response.finish_reasons"
	AttrUsageInputTokens      = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens     = "gen_ai.usage.output_tokens"
)

// llmtap-specific extensions. Names follow the gen_ai.* root so dashboards
// can use a single namespace; the suffix `.cost.usd` is not in the spec yet
// but matches community precedent (Logfire, Phoenix).
const (
	AttrCostUSD          = "gen_ai.cost.usd"
	AttrTimeToFirstToken = "gen_ai.time_to_first_token"
	AttrStream           = "gen_ai.request.stream"
)

// System values (gen_ai.system).
const (
	SystemOpenAI    = "openai"
	SystemAnthropic = "anthropic"
)

// Operation names (gen_ai.operation.name).
const (
	OpChat       = "chat"
	OpEmbeddings = "embeddings"
)

// Span event names (semconv GenAI events).
const (
	EventSystemMessage    = "gen_ai.system.message"
	EventUserMessage      = "gen_ai.user.message"
	EventAssistantMessage = "gen_ai.assistant.message"
	EventToolMessage      = "gen_ai.tool.message"
	EventChoice           = "gen_ai.choice"
)

// Metric instrument names (semconv GenAI metrics).
const (
	MetricTokenUsage        = "gen_ai.client.token.usage"
	MetricOperationDuration = "gen_ai.client.operation.duration"
	MetricTimeToFirstToken  = "gen_ai.client.time_to_first_token"
	MetricCostUSD           = "gen_ai.client.cost.usd" // llmtap extension
)

// Token-type label values for MetricTokenUsage.
const (
	TokenTypeInput  = "input"
	TokenTypeOutput = "output"
)

// SpanName builds the canonical GenAI span name: "{operation} {model}".
// Falls back gracefully if model is unknown so we still get useful spans.
func SpanName(op, model string) string {
	if model == "" {
		return op
	}
	return op + " " + model
}
