// Package provider parses upstream-LLM-API requests and responses into a
// vendor-neutral [Info] view, suitable for OTel GenAI semconv enrichment.
//
// Each implementation owns the wire details of one provider family. Adding a
// new family means writing one file plus a registration in [BuiltIn].
package provider

import (
	"io"
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
	System        string
	Operation     string
	RequestModel  string
	ResponseModel string
	ResponseID    string
	FinishReasons []string
	InputTokens   int64
	OutputTokens  int64
	Stream        bool
	Started       time.Time
	FirstByteAt   time.Time // wall-clock of first response byte; zero if not measured
	FirstTokenAt  time.Time // wall-clock of first content delta in stream; zero if non-streaming
	Finished      time.Time
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

// ContentOpts shapes how providers handle prompt/completion content
// when populating span events.
type ContentOpts struct {
	// Capture controls whether content events are emitted at all. When
	// false, providers attach only metadata (model, parameters, token
	// counts) — never content.
	Capture bool
	// Redact, if non-nil, is applied to every captured content string
	// before it reaches a span attribute. nil is interpreted as
	// passthrough (no redaction). Callers should populate this from
	// internal/redact.Func so the privacy default isn't "off by
	// accident".
	Redact func(string) string
}

// Clean returns s with the configured redactor applied, or s unchanged
// when Redact is nil. Providers should funnel every captured content
// string through this method on the way to a span attribute.
func (o ContentOpts) Clean(s string) string {
	if o.Redact == nil {
		return s
	}
	return o.Redact(s)
}

// Provider is the parser contract. Implementations are stateless; per-call
// state lives on [Info].
type Provider interface {
	// System returns the gen_ai.system value (e.g. "openai", "anthropic").
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
	// while parsing SSE events out-of-band. onFirstToken is invoked once
	// when the first content delta is observed; onDone is invoked exactly
	// once when the stream completes (EOF, error, or Close), with info
	// fully populated. Both callbacks must be cheap and non-blocking.
	WrapStream(
		span trace.Span,
		info *Info,
		body io.ReadCloser,
		content ContentOpts,
		onFirstToken func(),
		onDone func(),
	) io.ReadCloser
}

// Registry maps the provider name in config (config.Upstream.Provider) to its
// implementation. BuiltIn returns the v0.1 set.
type Registry map[string]Provider

// BuiltIn returns the registry of providers that ship with llmtap.
func BuiltIn() Registry {
	return Registry{
		"openai":    &OpenAI{},
		"anthropic": &Anthropic{},
	}
}
