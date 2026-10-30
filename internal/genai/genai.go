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
	AttrSystem                 = "gen_ai.provider.name"
	AttrOperationName          = "gen_ai.operation.name"
	AttrRequestModel           = "gen_ai.request.model"
	AttrRequestTemperature     = "gen_ai.request.temperature"
	AttrRequestTopP            = "gen_ai.request.top_p"
	AttrRequestTopK            = "gen_ai.request.top_k"
	AttrRequestMaxTokens       = "gen_ai.request.max_tokens"
	AttrRequestFrequencyPen    = "gen_ai.request.frequency_penalty"
	AttrRequestPresencePen     = "gen_ai.request.presence_penalty"
	AttrRequestSeed            = "gen_ai.request.seed"
	AttrRequestStopSequences   = "gen_ai.request.stop_sequences"
	AttrRequestEncodingFmts    = "gen_ai.request.encoding_formats"
	AttrResponseID             = "gen_ai.response.id"
	AttrResponseModel          = "gen_ai.response.model"
	AttrResponseFinishReasons  = "gen_ai.response.finish_reasons"
	AttrUsageInputTokens       = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens      = "gen_ai.usage.output_tokens"
	AttrUsageCacheReadTokens   = "gen_ai.usage.cache_read.input_tokens"
	AttrUsageCacheCreateTokens = "gen_ai.usage.cache_creation.input_tokens"
	AttrUsageReasoningTokens   = "gen_ai.usage.reasoning.output_tokens"
	AttrErrorType              = "error.type"
	AttrRequestServiceTier     = "gen_ai.openai.request.service_tier"
	AttrResponseServiceTier    = "gen_ai.openai.response.service_tier"
	AttrCaller                 = "llmtap.caller"
)

// llmtap-specific extensions. Names follow the gen_ai.* root so dashboards
// can use a single namespace; the suffix `.cost.usd` is not in the spec yet
// but matches community precedent (Logfire, Phoenix).
const (
	AttrCostUSD          = "gen_ai.cost.usd"
	AttrTimeToFirstToken = "gen_ai.time_to_first_token"
	AttrTimeToFirstByte  = "gen_ai.server.time_to_first_byte"
	AttrStream           = "gen_ai.request.stream"
)

// Provider values (gen_ai.provider.name).
const (
	SystemOpenAI    = "openai"
	SystemAnthropic = "anthropic"
	SystemGemini    = "gcp.gen_ai"
)

// Operation names (gen_ai.operation.name).
const (
	OpChat       = "chat"
	OpEmbeddings = "embeddings"
	// Responses API is a wire API, not a semantic operation. It emits chat.
	OpResponses = OpChat
)

// Span event names (semconv GenAI events).
const (
	EventSystemMessage    = "gen_ai.system.message"
	EventUserMessage      = "gen_ai.user.message"
	EventAssistantMessage = "gen_ai.assistant.message"
	EventToolMessage      = "gen_ai.tool.message"
	EventChoice           = "gen_ai.choice"
	EventToolCall         = "gen_ai.tool.call"
)

// Metric instrument names (semconv GenAI metrics).
const (
	MetricTokenUsage        = "gen_ai.client.token.usage"
	MetricOperationDuration = "gen_ai.client.operation.duration"
	MetricTimeToFirstToken  = "gen_ai.client.time_to_first_token"
	// MetricTimeToFirstByte is wall-clock from request start to the
	// first upstream response byte (headers/status known). Distinct
	// from TTFT, which waits for the first content/tool delta.
	MetricTimeToFirstByte = "gen_ai.server.time_to_first_byte"
	// MetricCostUSD is a USD-scaled histogram of per-call cost — gives
	// dashboards p50 / p95 cost per operation alongside a running sum.
	// Histogram buckets at 1e-5, 1e-4, 1e-3, 0.01, 0.1, 1, 10, 100 USD
	// span free-tier embeddings ($0.00002) through the most expensive
	// gpt-4-class single calls.
	MetricCostUSD = "gen_ai.client.cost.usd"
	// MetricCostUSDTotal is a monotonic counter of cumulative spend.
	// PromQL `sum(rate(gen_ai_client_cost_usd_total_total[5m])) * 3600`
	// reads as "USD/hour" — what FinOps dashboards actually plot.
	// Kept as a sibling rather than reading from the histogram's
	// _sum series because float-counter drift across millions of small
	// adds is a known concern.
	MetricCostUSDTotal = "gen_ai.client.cost.usd.total"
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
