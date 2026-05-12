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
	System         string
	Operation      string
	RequestModel   string
	ResponseModel  string
	ResponseID     string
	FinishReasons  []string
	InputTokens    int64
	OutputTokens   int64
	Stream         bool
	Started        time.Time
	FirstByteAt    time.Time // wall-clock of first response byte; zero if not measured
	FirstTokenAt   time.Time // wall-clock of first content delta in stream; zero if non-streaming
	Finished       time.Time
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
	// request. Implementations must not retain body. Setting captureContent
	// true allows attaching prompt content as span events; false (the
	// default) restricts attributes to metadata only.
	ParseRequest(span trace.Span, urlPath string, body []byte, captureContent bool) Info

	// ParseResponseJSON merges response fields into info from a complete,
	// non-streaming JSON body. It also decorates span with response
	// attributes and (when captureContent is true) the choice event.
	ParseResponseJSON(span trace.Span, info *Info, body []byte, captureContent bool)

	// WrapStream returns a reader that proxies body to the caller unchanged
	// while parsing SSE events out-of-band. onFirstToken is invoked once
	// when the first content delta is observed; onDone is invoked exactly
	// once when the stream completes (EOF, error, or Close), with info
	// fully populated. Both callbacks must be cheap and non-blocking.
	WrapStream(
		span trace.Span,
		info *Info,
		body io.ReadCloser,
		captureContent bool,
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
