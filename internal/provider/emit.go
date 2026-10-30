package provider

import (
	"github.com/colinedwardwood/llmtap/internal/genai"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// emitRequestAttrs writes the common GenAI request attributes and
// renames the span. Pure decode leaves info/Params filled; this is the
// span side-effect half.
func emitRequestAttrs(span trace.Span, info *Info) {
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrSystem, info.System),
		attribute.String(genai.AttrOperationName, info.Operation),
		attribute.Bool(genai.AttrStream, info.Stream),
	}
	if info.RequestModel != "" {
		attrs = append(attrs, attribute.String(genai.AttrRequestModel, info.RequestModel))
	}
	if info.ServiceTier != "" && info.API == APIResponses {
		attrs = append(attrs, attribute.String(genai.AttrRequestServiceTier, info.ServiceTier))
	}
	p := info.Params
	if p.Temperature != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestTemperature, *p.Temperature))
	}
	if p.TopP != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestTopP, *p.TopP))
	}
	if p.TopK != nil {
		attrs = append(attrs, attribute.Int64(genai.AttrRequestTopK, *p.TopK))
	}
	if p.MaxTokens != nil {
		attrs = append(attrs, attribute.Int64(genai.AttrRequestMaxTokens, *p.MaxTokens))
	}
	if p.FrequencyPenalty != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestFrequencyPen, *p.FrequencyPenalty))
	}
	if p.PresencePenalty != nil {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestPresencePen, *p.PresencePenalty))
	}
	if p.Seed != nil {
		attrs = append(attrs, attribute.Int64(genai.AttrRequestSeed, *p.Seed))
	}
	if len(p.StopSequences) > 0 {
		attrs = append(attrs, attribute.StringSlice(genai.AttrRequestStopSequences, p.StopSequences))
	}
	if len(p.EncodingFormats) > 0 {
		attrs = append(attrs, attribute.StringSlice(genai.AttrRequestEncodingFmts, p.EncodingFormats))
	}
	span.SetAttributes(attrs...)
	span.SetName(genai.SpanName(info.Operation, info.RequestModel))
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
	if info.CacheReadInputTokens > 0 {
		out = append(out, attribute.Int64(genai.AttrUsageCacheReadTokens, info.CacheReadInputTokens))
	}
	if info.CacheCreationInputTokens > 0 {
		out = append(out, attribute.Int64(genai.AttrUsageCacheCreateTokens, info.CacheCreationInputTokens))
	}
	if info.ReasoningOutputTokens > 0 {
		out = append(out, attribute.Int64(genai.AttrUsageReasoningTokens, info.ReasoningOutputTokens))
	}
	if info.ErrorType != "" {
		out = append(out, attribute.String(genai.AttrErrorType, info.ErrorType))
	}
	if info.ServiceTier != "" {
		out = append(out, attribute.String(genai.AttrResponseServiceTier, info.ServiceTier))
	}
	return out
}

func emitResponseAttrs(span trace.Span, info *Info) {
	span.SetAttributes(responseAttrs(info)...)
}
