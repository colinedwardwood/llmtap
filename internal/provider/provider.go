// Package provider parses upstream-LLM-API requests and responses into a
// vendor-neutral [Info] view, suitable for OTel GenAI semconv enrichment.
//
// Each implementation owns the wire details of one provider family. Adding a
// new family means writing one file plus a registration in [BuiltIn].
//
// Parsing is deliberately split from span emission:
//
//	decode*() → typed structs / [Info]  (pure, span-free — record/replay)
//	emit*()   → span attributes/events  (side-effecting)
//
// The [Provider] methods compose decode+emit so the proxy keeps a single
// call site. Tests (and future B2 record/replay) can call the decode
// helpers directly.
package provider

import (
	"io"
	"sort"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Info is the vendor-neutral view of one LLM call. Fields are populated as
// information becomes available: request fields after parsing the request,
// response fields after parsing a non-streaming JSON body or after the SSE
// stream completes.
//
// Zero values mean "unknown" (e.g. tokens=0 from a 4xx, finish reasons empty
// when streaming aborted). The proxy reports only fields that are known.
type Info struct {
	System                   string
	API                      string
	Operation                string
	RequestModel             string
	ResponseModel            string
	ResponseID               string
	FinishReasons            []string
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	ReasoningOutputTokens    int64
	ErrorType                string
	ServiceTier              string
	Refusal                  string
	Caller                   string
	Stream                   bool
	// Params holds request sampling / generation knobs decoded from
	// the body. Kept as a bag so emit and cost-weighted sampling can
	// read them without re-parsing the wire format.
	Params       RequestParams
	Started      time.Time
	FirstByteAt  time.Time // wall-clock of first response byte; zero if not measured
	FirstTokenAt time.Time // wall-clock of first content delta in stream; zero if non-streaming
	Finished     time.Time
}

// RequestParams is the provider-neutral bag of generation knobs. Pointer
// and slice fields distinguish "absent" from "explicitly zero" / empty.
type RequestParams struct {
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	MaxTokens        *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	Seed             *int64
	StopSequences    []string
	EncodingFormats  []string
}

// DurationSeconds returns Finished-Started in seconds; 0 if either is zero.
func (i Info) DurationSeconds() float64 {
	if i.Started.IsZero() || i.Finished.IsZero() {
		return 0
	}
	return i.Finished.Sub(i.Started).Seconds()
}

// TimeToFirstTokenSeconds returns FirstTokenAt-Started in seconds; 0 if not
// applicable (non-streaming response or no tokens received).
func (i Info) TimeToFirstTokenSeconds() float64 {
	if i.Started.IsZero() || i.FirstTokenAt.IsZero() {
		return 0
	}
	return i.FirstTokenAt.Sub(i.Started).Seconds()
}

// TimeToFirstByteSeconds returns FirstByteAt-Started in seconds; 0 if
// ModifyResponse never observed an upstream response.
func (i Info) TimeToFirstByteSeconds() float64 {
	if i.Started.IsZero() || i.FirstByteAt.IsZero() {
		return 0
	}
	return i.FirstByteAt.Sub(i.Started).Seconds()
}

// ContentOpts shapes how providers handle prompt/completion content.
// See capture.go for the struct definition and emit helpers.

// Provider is the parser contract. Implementations are stateless; per-call
// state lives on [Info]. Parse*/WrapStream compose decode + emit so the
// proxy has a single call site; decode helpers are available for pure
// parsing (tests, record/replay).
type Provider interface {
	// System returns the gen_ai.provider.name value (e.g. "openai", "anthropic").
	System() string

	// OperationFor maps a request path to the GenAI operation name. Returns
	// the empty string when the path is not a recognised LLM endpoint, in
	// which case the proxy will pass the request through without enrichment.
	OperationFor(urlPath string) string

	// ParseRequest decorates span and returns a populated [Info] for the
	// request. Implementations must not retain body. The ContentOpts
	// argument governs whether prompt content is attached as span
	// events and how it is scrubbed before attachment.
	ParseRequest(span trace.Span, urlPath string, body []byte, content ContentOpts) Info

	// ParseResponseJSON merges response fields into info from a complete,
	// non-streaming JSON body. It also decorates span with response
	// attributes and (when content.Capture is true) the choice event.
	ParseResponseJSON(span trace.Span, info *Info, body []byte, content ContentOpts)

	// WrapStream returns a reader that proxies body to the caller unchanged
	// while parsing SSE events out-of-band. onDone is invoked exactly
	// once when the stream completes (EOF, error, or Close), with info
	// fully populated. The callback must be cheap and non-blocking.
	// First-token bookkeeping (TTFT) is internal to the stream collector.
	WrapStream(
		span trace.Span,
		info *Info,
		body io.ReadCloser,
		content ContentOpts,
		onDone func(),
	) io.ReadCloser
}

// Registry maps the provider name in config (config.Upstream.Provider) to its
// implementation. BuiltIn returns the v0.1 set.
type Registry map[string]Provider

const (
	APIChatCompletions = "chat_completions"
	APIResponses       = "responses"
	APIEmbeddings      = "embeddings"
)

// BuiltIn returns the registry of providers that ship with llmtap.
func BuiltIn() Registry {
	return Registry{
		"openai":    &OpenAI{},
		"anthropic": &Anthropic{},
		"gemini":    &Gemini{},
	}
}

// Known reports whether name is a built-in provider. Config validation
// uses this instead of hardcoding the name list, so adding a provider
// to BuiltIn is the only registration step.
func Known(name string) bool {
	_, ok := BuiltIn()[name]
	return ok
}

// KnownNames returns the sorted list of built-in provider names for
// error messages.
func KnownNames() []string {
	r := BuiltIn()
	names := make([]string, 0, len(r))
	for n := range r {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
