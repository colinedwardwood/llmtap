package proxy

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// idleTimeoutBody wraps a response body and closes it if no Read
// observes n > 0 bytes within idle. Post-header only: the wrapper is
// installed in ModifyResponse after status/headers are known.
//
// Timer reset happens on every successful byte delivery. Zero idle
// (or a nil body) is a no-op pass-through constructed by the caller
// not wrapping at all.
type idleTimeoutBody struct {
	body   io.ReadCloser
	idle   time.Duration
	onIdle func()

	mu     sync.Mutex
	timer  *time.Timer
	fired  atomic.Bool
	closed atomic.Bool
}

func wrapIdleTimeout(body io.ReadCloser, idle time.Duration, onIdle func()) io.ReadCloser {
	if body == nil || idle <= 0 {
		return body
	}
	w := &idleTimeoutBody{body: body, idle: idle, onIdle: onIdle}
	w.timer = time.AfterFunc(idle, w.fire)
	return w
}

func (w *idleTimeoutBody) fire() {
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.onIdle != nil {
		w.onIdle()
	}
	_ = w.closeBody()
}

func (w *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	if n > 0 && !w.fired.Load() {
		w.mu.Lock()
		if w.timer != nil {
			w.timer.Reset(w.idle)
		}
		w.mu.Unlock()
	}
	return n, err
}

func (w *idleTimeoutBody) Close() error {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
	return w.closeBody()
}

func (w *idleTimeoutBody) closeBody() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	return w.body.Close()
}

// IdleFired reports whether the idle watchdog closed the body.
func (w *idleTimeoutBody) IdleFired() bool { return w.fired.Load() }
