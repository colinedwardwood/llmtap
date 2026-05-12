package provider

import (
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

func TestSSETeeFlushesTrailingPartial(t *testing.T) {
	t.Parallel()

	src := io.NopCloser(strings.NewReader("data: tail-without-blank-line"))
	var got []captured
	tee := newSSETee(src,
		func(event string, data []byte) {
			got = append(got, captured{event: event, data: string(data)})
		},
		func() {},
	)
	if _, err := io.ReadAll(tee); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].data != "tail-without-blank-line" {
		t.Fatalf("trailing partial not flushed: %+v", got)
	}
}
