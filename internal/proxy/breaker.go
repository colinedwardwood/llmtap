package proxy

import (
	"sync"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
)

// breakerState is the three-state machine: closed (passthrough),
// open (all admissions short-circuit to 503), half-open (exactly one
// probe in flight, all other admissions reject).
type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// breaker is a per-upstream consecutive-failure circuit breaker. It is
// intentionally minimal: no leaky-bucket math, no exponential backoff,
// no token-bucket admission control. Operators tune the failure count
// and recovery window in config; the breaker is a fuse, not a load
// shedder.
//
// Concurrency model: a single sync.Mutex serialises every state read
// and write. Admission checks and outcome reports both go through
// admit() and report(), so there is one path into the state machine.
// Lock-free designs are tempting but the breaker is checked once per
// request, downstream of much more expensive work — the contention
// budget is generous.
type breaker struct {
	cfg config.BreakerConfig

	mu sync.Mutex
	// state is the current FSM position.
	state breakerState
	// consecutive counts non-reset 5xx since the breaker last closed
	// or trip-recovered. Reset to zero on any non-5xx outcome.
	consecutive int
	// lastFailure records the timestamp of the most recent counted
	// failure. Used to invalidate the consecutive count when Window
	// is set and the gap between failures exceeds Window.
	lastFailure time.Time
	// openedAt is when the breaker transitioned closed -> open. Used
	// to time RecoveryWindow.
	openedAt time.Time
	// probeInFlight is true once half-open has admitted its single
	// probe; further requests reject until the probe resolves.
	probeInFlight bool

	// now is injected for tests; production uses time.Now.
	now func() time.Time
}

// newBreaker constructs a breaker from validated config. The caller is
// responsible for refusing to construct when cfg.Failures <= 0.
func newBreaker(cfg config.BreakerConfig) *breaker {
	if cfg.RecoveryWindow <= 0 {
		if cfg.Window > 0 {
			cfg.RecoveryWindow = cfg.Window
		} else {
			cfg.RecoveryWindow = 15 * time.Second
		}
	}
	return &breaker{cfg: cfg, now: time.Now}
}

// admit decides whether to forward the request to the upstream. The
// boolean return is true when the caller may proceed. When false, the
// caller must respond 503 + Retry-After: <secs> and skip the upstream
// entirely. The retryAfter return is the number of seconds the client
// should wait before retrying; only meaningful when admit returns
// false.
func (b *breaker) admit() (ok bool, retryAfter int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerClosed:
		return true, 0

	case breakerOpen:
		// Has the recovery window elapsed? If so, transition to
		// half-open and admit this request as the probe.
		if b.now().Sub(b.openedAt) >= b.cfg.RecoveryWindow {
			b.state = breakerHalfOpen
			b.probeInFlight = true
			return true, 0
		}
		// Still open; tell the client when to retry.
		retry := b.cfg.RecoveryWindow - b.now().Sub(b.openedAt)
		secs := int(retry.Seconds())
		if secs < 1 {
			secs = 1
		}
		return false, secs

	case breakerHalfOpen:
		// Exactly one probe is permitted in flight. Any subsequent
		// request rejects with the recovery window's worth of
		// Retry-After. Once the probe resolves via report(), the
		// state transitions back to closed or open.
		if !b.probeInFlight {
			b.probeInFlight = true
			return true, 0
		}
		secs := int(b.cfg.RecoveryWindow.Seconds())
		if secs < 1 {
			secs = 1
		}
		return false, secs
	}
	return true, 0
}

// report folds a completed request's status code into the breaker
// state. Only 5xx counts as a failure. Any non-5xx (including 4xx,
// 3xx, 2xx) is treated as a success — 4xx is the upstream telling
// the proxy "your request was bad," which is not an upstream-side
// fault.
func (b *breaker) report(statusCode int) {
	failure := statusCode >= 500 && statusCode <= 599

	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerHalfOpen:
		// The half-open probe resolved. Probes are the only requests
		// the state machine cares about while half-open.
		b.probeInFlight = false
		if failure {
			b.trip()
			return
		}
		// Probe succeeded — reset to closed and zero counters.
		b.state = breakerClosed
		b.consecutive = 0
		b.lastFailure = time.Time{}
		return

	case breakerClosed:
		if failure {
			now := b.now()
			// If Window is set and the gap since the last failure
			// exceeds Window, the consecutive run is broken: this
			// failure starts a new run of one.
			if b.cfg.Window > 0 && !b.lastFailure.IsZero() && now.Sub(b.lastFailure) > b.cfg.Window {
				b.consecutive = 0
			}
			b.consecutive++
			b.lastFailure = now
			if b.consecutive >= b.cfg.Failures {
				b.trip()
			}
			return
		}
		// Non-5xx in closed state clears the consecutive count.
		b.consecutive = 0
		b.lastFailure = time.Time{}

	case breakerOpen:
		// While open, the proxy short-circuits before reaching the
		// upstream — report() should rarely be called in this
		// state. If a stale in-flight request reports a 5xx, fold
		// it in by extending openedAt; if non-5xx, ignore (the
		// breaker is not closed yet, the probe path will resolve
		// it).
		if failure {
			b.openedAt = b.now()
		}
	}
}

// trip moves the breaker into open. Caller must hold b.mu.
func (b *breaker) trip() {
	b.state = breakerOpen
	b.openedAt = b.now()
	b.probeInFlight = false
	b.consecutive = 0
}
