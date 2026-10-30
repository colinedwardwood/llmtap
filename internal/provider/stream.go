package provider

import (
	"context"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// streamCollector factors the WrapStream mechanics shared by every
// provider: first-token bookkeeping, capped content assembly, and the
// close-once finalize that emits response attrs + the choice event.
// Providers supply only an onEvent callback that mutates info / calls
// noteFirstToken / appendText.
type streamCollector struct {
	span      trace.Span
	info      *Info
	content   ContentOpts
	assembled cappedBuilder
	gotFirst  bool
	onDone    func()
	tee       *sseTee
}

func newStreamCollector(span trace.Span, info *Info, content ContentOpts, onDone func()) *streamCollector {
	return &streamCollector{
		span:    span,
		info:    info,
		content: content,
		onDone:  onDone,
	}
}

// noteFirstToken records TTFT on the first generating delta (text or
// tool-call). Idempotent.
func (c *streamCollector) noteFirstToken() {
	if c.gotFirst {
		return
	}
	c.gotFirst = true
	c.info.FirstTokenAt = time.Now()
}

// appendText accumulates completion text when content capture is on.
func (c *streamCollector) appendText(delta string) {
	if c.content.Capture() && delta != "" {
		c.assembled.WriteString(delta)
	}
}

// close finalizes the stream exactly once: stamps Finished, emits
// response attributes / choice event / overflow count, then runs onDone.
func (c *streamCollector) close() {
	if !c.info.Finished.IsZero() {
		return
	}
	c.info.Finished = time.Now()
	emitResponseAttrs(c.span, c.info)
	if c.content.Capture() && c.assembled.Len() > 0 {
		ctx := trace.ContextWithSpan(context.Background(), c.span)
		c.content.emitAssembledChoice(ctx, c.span, c.assembled.String(), c.assembled.Truncated())
	}
	if c.tee != nil {
		if n := c.tee.Overflows(); n > 0 {
			c.span.SetAttributes(attribute.Int("llmtap.sse_parser_overflows", n))
		}
	}
	if c.onDone != nil {
		c.onDone()
	}
}

// wrap starts the SSE tee with the given event handler and returns the
// ReadCloser the proxy installs as the response body.
func (c *streamCollector) wrap(body io.ReadCloser, onEvent sseEventHandler) io.ReadCloser {
	c.tee = newSSETee(body, onEvent, nil, c.close)
	return c.tee
}

// sseEventHandler matches newSSETee's onEvent signature.
type sseEventHandler func(event string, data []byte)
