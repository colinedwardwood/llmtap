package provider

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

type captured struct {
	event string
	data  string
}

func TestSSETeeForwardsAndParses(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"a"}}`,
		"",
		"event: content_block_delta",
		`data: {"delta":{"text":"hello"}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	src := io.NopCloser(strings.NewReader(body))

	var got []captured
	closed := 0
	tee := newSSETee(src,
		func(event string, data []byte) {
			got = append(got, captured{event: event, data: string(data)})
		},
		nil, // onOverflow
		func() { closed++ },
	)

	out, err := io.ReadAll(tee)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != body {
		t.Errorf("forwarded bytes diverged from source")
	}
	if got[0].event != "message_start" || !strings.Contains(got[0].data, `"id":"a"`) {
		t.Errorf("first event = %+v", got[0])
	}
	if got[1].event != "content_block_delta" || !strings.Contains(got[1].data, `hello`) {
		t.Errorf("second event = %+v", got[1])
	}
	if got[2].data != "[DONE]" {
		t.Errorf("done sentinel = %q", got[2].data)
	}
	// onClose must fire on EOF; calling Close again must not double-fire.
	if err := tee.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed != 1 {
		t.Errorf("onClose called %d times, want 1", closed)
	}
}

// TestSSETeeBoundsBufferUnderOverflow is the A13 regression. A
// pathological upstream that streams MiB of bytes without ever
// emitting a `\n\n` event terminator must not OOM the proxy. Bytes
// still forward to the client unchanged; parsing is allowed to give
// up via a one-shot overflow signal.
func TestSSETeeBoundsBufferUnderOverflow(t *testing.T) {
	t.Parallel()

	// 10 MiB of payload, no event separators anywhere.
	payload := bytes.Repeat([]byte("x"), 10*1024*1024)
	src := io.NopCloser(bytes.NewReader(payload))

	var overflows int
	tee := newSSETee(src,
		func(event string, data []byte) {},
		func() { overflows++ },
		func() {})

	out, err := io.ReadAll(tee)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(payload) {
		t.Errorf("forwarded %d bytes, want %d (proxy must not drop client bytes on parser overflow)", len(out), len(payload))
	}
	if overflows == 0 {
		t.Error("expected onOverflow callback to fire at least once on no-separator payload")
	}
}

// TestSSETeeResumesParsingAfterOverflow asserts that the parser
// recovers once the upstream resumes emitting proper SSE frames. The
// overflow is a soft state — drop the unparseable accumulator,
// keep parsing what comes next.
func TestSSETeeResumesParsingAfterOverflow(t *testing.T) {
	t.Parallel()

	junk := bytes.Repeat([]byte("x"), 2*1024*1024) // > maxEventBytes
	frame := []byte("\n\ndata: hello\n\n")
	src := io.NopCloser(bytes.NewReader(append(junk, frame...)))

	var (
		overflows int
		got       []captured
	)
	tee := newSSETee(src,
		func(event string, data []byte) {
			got = append(got, captured{event: event, data: string(data)})
		},
		func() { overflows++ },
		func() {})

	if _, err := io.ReadAll(tee); err != nil {
		t.Fatal(err)
	}
	if overflows == 0 {
		t.Error("expected overflow to fire for the junk prefix")
	}
	var foundHello bool
	for _, ev := range got {
		if ev.data == "hello" {
			foundHello = true
		}
	}
	if !foundHello {
		t.Errorf("post-overflow event \"hello\" not dispatched; got %+v", got)
	}
}

// TestSSETeeFramingCRLF is the A21 regression. The SSE spec (HTML5
// §EventSource) allows `\n\n`, `\r\n\r\n`, or `\r\r` as a frame
// terminator. Today only `\n\n` is recognized; if OpenAI or Anthropic
// switch to CRLF framing (or proxy via a CRLF-rewriting intermediary)
// llmtap silently degrades to "no events ever parsed."
func TestSSETeeFramingCRLF(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"event: message_start",
		`data: {"id":"a"}`,
		"",
		"event: content_block_delta",
		`data: {"text":"hello"}`,
		"",
		"data: [DONE]",
		"",
	}, "\r\n")

	src := io.NopCloser(strings.NewReader(body))
	var got []captured
	tee := newSSETee(src,
		func(event string, data []byte) {
			got = append(got, captured{event: event, data: string(data)})
		},
		nil, func() {},
	)
	if _, err := io.ReadAll(tee); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("CRLF framing produced %d events, want 3: %+v", len(got), got)
	}
	if got[0].event != "message_start" || !strings.Contains(got[0].data, `"id":"a"`) {
		t.Errorf("first event = %+v", got[0])
	}
	if got[1].event != "content_block_delta" || !strings.Contains(got[1].data, "hello") {
		t.Errorf("second event = %+v", got[1])
	}
	if got[2].data != "[DONE]" {
		t.Errorf("done sentinel = %q", got[2].data)
	}
}

// TestSSETeeFramingCR covers the legacy `\r\r` terminator (HTML5 also
// permits this for ancient Mac-line-ending content).
func TestSSETeeFramingCR(t *testing.T) {
	t.Parallel()

	body := "event: ping\rdata: one\r\rdata: two\r\r"
	src := io.NopCloser(strings.NewReader(body))
	var got []captured
	tee := newSSETee(src,
		func(event string, data []byte) {
			got = append(got, captured{event: event, data: string(data)})
		},
		nil, func() {},
	)
	if _, err := io.ReadAll(tee); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("CR framing produced %d events, want 2: %+v", len(got), got)
	}
	if got[0].event != "ping" || got[0].data != "one" {
		t.Errorf("first event = %+v", got[0])
	}
	if got[1].data != "two" {
		t.Errorf("second event = %+v", got[1])
	}
}

// TestSSETeeFramingMixed covers a stream that interleaves LF, CRLF,
// and CR terminators — e.g. an upstream + an intermediary disagreeing
// on line endings. Each event should still dispatch at its boundary.
func TestSSETeeFramingMixed(t *testing.T) {
	t.Parallel()

	// Frame 1: LF-LF. Frame 2: CRLF-CRLF. Frame 3: CR-CR.
	body := "data: one\n\n" +
		"data: two\r\n\r\n" +
		"data: three\r\r"
	src := io.NopCloser(strings.NewReader(body))
	var got []captured
	tee := newSSETee(src,
		func(event string, data []byte) {
			got = append(got, captured{event: event, data: string(data)})
		},
		nil, func() {},
	)
	if _, err := io.ReadAll(tee); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("mixed framing produced %d events, want 3: %+v", len(got), got)
	}
	want := []string{"one", "two", "three"}
	for i, w := range want {
		if got[i].data != w {
			t.Errorf("event %d data = %q, want %q", i, got[i].data, w)
		}
	}
}

func TestSSETeeFlushesTrailingPartial(t *testing.T) {
	t.Parallel()

	src := io.NopCloser(strings.NewReader("data: tail-without-blank-line"))
	var got []captured
	tee := newSSETee(src,
		func(event string, data []byte) {
			got = append(got, captured{event: event, data: string(data)})
		},
		nil, // onOverflow
		func() {},
	)
	if _, err := io.ReadAll(tee); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].data != "tail-without-blank-line" {
		t.Fatalf("trailing partial not flushed: %+v", got)
	}
}
