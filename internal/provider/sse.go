package provider

import (
	"bytes"
	"io"
	"sync"
)

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
	onClose func()
	once    sync.Once
}

func newSSETee(src io.ReadCloser, onEvent func(event string, data []byte), onClose func()) *sseTee {
	return &sseTee{src: src, onEvent: onEvent, onClose: onClose}
}

func (t *sseTee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		_, _ = t.buf.Write(p[:n])
		t.drain(false)
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

// drain extracts complete SSE messages (delimited by a blank line) from the
// buffer. When eof is true, any trailing message without a terminating blank
// line is also flushed.
func (t *sseTee) drain(eof bool) {
	for {
		raw := t.buf.Bytes()
		idx := bytes.Index(raw, []byte("\n\n"))
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
		t.buf.Next(idx + 2)
		t.dispatch(msg)
	}
}

func (t *sseTee) dispatch(msg []byte) {
	var (
		event string
		data  []byte
	)
	for _, line := range bytes.Split(msg, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
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
