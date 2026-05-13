package provider

import (
	"bytes"
	"io"
	"sync"
)

// maxEventBytes caps the in-flight SSE-parse buffer per stream. Real
// provider events are kilobytes at most; anything bigger is a sign of
// an upstream stuck in a non-SSE state (or actively trying to OOM the
// proxy). On overflow the buffer is dropped and onOverflow fires; bytes
// keep forwarding to the client unchanged — only the parser gives up.
const maxEventBytes = 1024 * 1024 // 1 MiB

// sseTee wraps an upstream io.ReadCloser, forwarding bytes unchanged to the
// caller while parsing SSE messages out-of-band. It guarantees onClose runs
// exactly once: whichever of EOF, transport error, or Close arrives first.
//
// Why a tee rather than a goroutine + io.Pipe: the proxy already runs in the
// request goroutine; staying single-threaded avoids a goroutine per in-flight
// request and removes a class of leak.
type sseTee struct {
	src     io.ReadCloser
	buf     bytes.Buffer
	onEvent func(event string, data []byte)
	// onOverflow fires every time the parser buffer crosses
	// maxEventBytes. A one-shot signal hides sustained pathological
	// streams (a 100 MiB no-separator payload looked identical to a
	// 2 MiB one); firing each ~1 MiB chunk gives operators a real
	// "this stream is still pathological" signal without flooding —
	// the cap is the overflow rate, not the call count. Callers
	// dedupe at the span/metric layer if they need to.
	onOverflow func()
	onClose    func()
	once       sync.Once
	overflows  int // total overflow events on this stream; exported via Overflows().
}

func newSSETee(src io.ReadCloser, onEvent func(event string, data []byte), onOverflow, onClose func()) *sseTee {
	return &sseTee{src: src, onEvent: onEvent, onOverflow: onOverflow, onClose: onClose}
}

func (t *sseTee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		_, _ = t.buf.Write(p[:n])
		if t.buf.Len() > maxEventBytes {
			// Pathological / hostile stream — drop the accumulator
			// and announce overflow. Bytes still flow to the client
			// via the n we return; only the SSE parser gives up on
			// this chunk. Firing per-overflow (vs. once-per-stream)
			// surfaces sustained hostile streams to operators.
			t.buf.Reset()
			t.overflows++
			if t.onOverflow != nil {
				t.onOverflow()
			}
		} else {
			t.drain(false)
		}
	}
	if err != nil {
		t.drain(true)
		t.once.Do(t.onClose)
	}
	return n, err
}

func (t *sseTee) Close() error {
	t.once.Do(t.onClose)
	return t.src.Close()
}

// Overflows returns the total number of buffer-overflow events on this
// stream. Useful in onClose to record a single span attribute summarising
// "this stream was pathological N times" rather than emitting N span
// events. Always safe to call after the stream is closed; reads on an
// in-flight stream race with Read and should be avoided.
func (t *sseTee) Overflows() int { return t.overflows }

// drain extracts complete SSE messages (delimited by a blank line) from the
// buffer. When eof is true, any trailing message without a terminating blank
// line is also flushed.
//
// Per the HTML5 EventSource spec (§9.2 "Parsing an event stream") a blank
// line may be `\n\n`, `\r\n\r\n`, or `\r\r` — any of the three sequences is
// a valid frame terminator. OpenAI / Anthropic both ship `\n\n` today, but a
// CRLF-rewriting intermediary (Cloudflare in some modes, Squid, certain
// corporate egress proxies) can rewrite to `\r\n\r\n` mid-flight. Recognize
// all three to avoid a silent "no events ever parsed" degrade.
func (t *sseTee) drain(eof bool) {
	for {
		raw := t.buf.Bytes()
		idx, sep := findFrameBoundary(raw)
		if idx < 0 {
			if !eof || t.buf.Len() == 0 {
				return
			}
			msg := t.buf.Bytes()
			t.buf.Reset()
			t.dispatch(msg)
			return
		}
		msg := make([]byte, idx)
		copy(msg, raw[:idx])
		t.buf.Next(idx + sep)
		t.dispatch(msg)
	}
}

// findFrameBoundary returns the offset of the first frame terminator and
// the length of the terminator (in bytes). Returns -1 if no terminator is
// present. Recognized terminators, longest first to disambiguate the case
// where `\r\n\r\n` would also match a shorter `\n\n` starting one byte in:
//
//	\r\n\r\n  (4 bytes)
//	\n\n      (2 bytes)
//	\r\r      (2 bytes)
func findFrameBoundary(raw []byte) (idx, sepLen int) {
	bestIdx := -1
	bestLen := 0
	for _, t := range []struct {
		sep []byte
	}{
		{[]byte("\r\n\r\n")},
		{[]byte("\n\n")},
		{[]byte("\r\r")},
	} {
		i := bytes.Index(raw, t.sep)
		if i < 0 {
			continue
		}
		if bestIdx < 0 || i < bestIdx {
			bestIdx = i
			bestLen = len(t.sep)
		}
	}
	return bestIdx, bestLen
}

func (t *sseTee) dispatch(msg []byte) {
	var (
		event string
		data  []byte
	)
	for _, line := range splitSSELines(msg) {
		switch {
		case len(line) == 0, bytes.HasPrefix(line, []byte(":")):
			// blank line / comment — ignore.
		case bytes.HasPrefix(line, []byte("event:")):
			event = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimSpace(line[len("data:"):])...)
		}
	}
	if len(data) == 0 && event == "" {
		return
	}
	t.onEvent(event, data)
}

// splitSSELines splits a frame body on any of `\r\n`, `\n`, or `\r`. The
// HTML5 EventSource spec defines a line terminator within a frame the same
// way it defines a frame terminator: any of the three sequences works. The
// previous implementation split on `\n` and trimmed a trailing `\r`, which
// produced one giant unsplittable line for CR-only payloads.
func splitSSELines(msg []byte) [][]byte {
	out := make([][]byte, 0, 4)
	start := 0
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c != '\n' && c != '\r' {
			continue
		}
		out = append(out, msg[start:i])
		// CRLF counts as one terminator.
		if c == '\r' && i+1 < len(msg) && msg[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	if start < len(msg) {
		out = append(out, msg[start:])
	}
	return out
}
