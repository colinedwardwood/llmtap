package proxy

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stallReader struct {
	first []byte
	sent  atomic.Bool
	block chan struct{}
}

func (s *stallReader) Read(p []byte) (int, error) {
	if !s.sent.Swap(true) {
		n := copy(p, s.first)
		return n, nil
	}
	<-s.block
	return 0, io.EOF
}

func (s *stallReader) Close() error {
	close(s.block)
	return nil
}

func TestIdleTimeoutBodyFiresOnStall(t *testing.T) {
	t.Parallel()
	src := &stallReader{first: []byte("hi"), block: make(chan struct{})}
	var idleFired atomic.Bool
	body := wrapIdleTimeout(src, 40*time.Millisecond, func() { idleFired.Store(true) })

	buf := make([]byte, 8)
	n, err := body.Read(buf)
	if err != nil || n != 2 || string(buf[:n]) != "hi" {
		t.Fatalf("first read: n=%d err=%v data=%q", n, err, buf[:n])
	}

	// Second Read blocks until the idle timer closes the body.
	done := make(chan struct{})
	go func() {
		_, _ = body.Read(buf)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle timeout did not unblock Read")
	}
	if !idleFired.Load() {
		t.Fatal("onIdle callback was not invoked")
	}
	w, ok := body.(*idleTimeoutBody)
	if !ok || !w.IdleFired() {
		t.Fatal("IdleFired not set")
	}
}

func TestIdleTimeoutBodyDisabled(t *testing.T) {
	t.Parallel()
	r := io.NopCloser(strings.NewReader("abc"))
	if got := wrapIdleTimeout(r, 0, nil); got != r {
		t.Fatal("zero idle should return original body")
	}
}

func TestIdleTimeoutBodyResetsOnBytes(t *testing.T) {
	t.Parallel()
	// Slow drip of single bytes spaced under the idle window must
	// complete without firing.
	pr, pw := io.Pipe()
	var idleFired atomic.Bool
	body := wrapIdleTimeout(pr, 80*time.Millisecond, func() { idleFired.Store(true) })

	go func() {
		defer pw.Close()
		for i := 0; i < 5; i++ {
			_, _ = pw.Write([]byte("x"))
			time.Sleep(25 * time.Millisecond)
		}
	}()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "xxxxx" {
		t.Fatalf("got %q", got)
	}
	if idleFired.Load() {
		t.Fatal("idle fired during active drip")
	}
	_ = body.Close()
}
