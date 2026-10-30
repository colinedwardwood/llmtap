package provider

import "strings"

// maxAssembledBytes bounds the per-stream completion-text accumulator
// used when content capture is on. Model output is nominally bounded by
// max_tokens, but the proxy must not trust the upstream to enforce
// that — a misbehaving (or malicious) SSE stream could otherwise grow
// a span attribute without limit. 64 KiB matches the tracer provider's
// attribute-value length limit, so nothing above this survives export
// anyway.
const maxAssembledBytes = 64 * 1024

// cappedBuilder is a strings.Builder with a hard byte ceiling. Writes
// past the cap are dropped (never split mid-rune) and remembered, so
// the caller can attach a truncation marker to the span instead of
// silently shipping a partial transcript as if it were complete.
type cappedBuilder struct {
	b         strings.Builder
	truncated bool
}

func (c *cappedBuilder) WriteString(s string) {
	if s == "" {
		return
	}
	remaining := maxAssembledBytes - c.b.Len()
	if remaining <= 0 {
		c.truncated = true
		return
	}
	if len(s) > remaining {
		// Trim back to a rune boundary so the captured text stays
		// valid UTF-8.
		n := remaining
		for n > 0 && (s[n]&0xC0) == 0x80 {
			n--
		}
		s = s[:n]
		c.truncated = true
	}
	c.b.WriteString(s)
}

func (c *cappedBuilder) Len() int        { return c.b.Len() }
func (c *cappedBuilder) String() string  { return c.b.String() }
func (c *cappedBuilder) Truncated() bool { return c.truncated }
