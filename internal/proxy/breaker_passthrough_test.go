package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// driveBreakerToHalfOpen trips the breaker via N failures, waits the
// recovery window, and returns. Callers can then exercise the
// half-open admission path with a single request.
func driveBreakerToHalfOpen(t *testing.T, clientURL string, failures int, recovery time.Duration) {
	t.Helper()
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < failures; i++ {
		resp, err := http.Post(clientURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("trip request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	// Wait for the recovery window so the next admit transitions to
	// half-open.
	time.Sleep(recovery + 10*time.Millisecond)
}

// TestBreakerProbeReleasedOnTransparentPassthrough is the C1 regression
// for the OperationFor=="" passthrough path. When the breaker is in
// half-open and the request resolves via the transparent (no-
// enrichment) passthrough, the probe slot must be released — otherwise
// every subsequent request 503s forever.
func TestBreakerProbeReleasedOnTransparentPassthrough(t *testing.T) {
	t.Parallel()

	const recovery = 30 * time.Millisecond
	var fail atomic.Bool
	fail.Store(true)
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 2, RecoveryWindow: recovery},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Trip the breaker via 2x 5xx on the enriched path.
	driveBreakerToHalfOpen(t, ts.URL, 2, recovery)

	// Flip upstream to healthy so the half-open probe succeeds.
	fail.Store(false)

	// The probe request hits a path the OpenAI provider does NOT
	// recognise (/v1/files). The path matches the upstream prefix,
	// so it goes through the breaker admission gate, but
	// provider.OperationFor returns "" — the proxy serves it via
	// transparent passthrough and the interceptor (which calls
	// br.report) is never wired up. The probe slot MUST still be
	// released on the way out.
	probe, err := http.Get(ts.URL + "/v1/files")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, probe.Body)
	_ = probe.Body.Close()
	if probe.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("probe via transparent passthrough returned 503; should be admitted as the half-open probe")
	}

	// Next request to a recognised path MUST be admitted. If the
	// breaker is stuck in half-open with probeInFlight=true, this
	// returns 503.
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Errorf("post-passthrough request returned 503; breaker is stuck (transparent passthrough didn't release the probe slot)")
	}
}

// TestBreakerProbeReleasedOn413 is the C1 regression for the hard-cap
// rejection path. When the breaker is half-open and the request's
// advertised Content-Length is over the hard cap, the proxy answers
// 413 without contacting the upstream — but the probe slot must be
// released, otherwise the next legitimate request gets 503.
func TestBreakerProbeReleasedOn413(t *testing.T) {
	t.Parallel()

	const recovery = 30 * time.Millisecond
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Request.MaxBodyBytes = 1024
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 2, RecoveryWindow: recovery},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Trip the breaker.
	driveBreakerToHalfOpen(t, ts.URL, 2, recovery)

	// Probe candidate: a request with Content-Length above the hard
	// cap. The proxy rejects with 413 BEFORE contacting upstream.
	big := bytes.Repeat([]byte{'a'}, 4*1024)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized probe status = %d; want 413", resp.StatusCode)
	}

	// Next legitimate request MUST be admitted (probe slot released
	// even though no upstream call was made).
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp2, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode == http.StatusServiceUnavailable {
		t.Errorf("post-413 request returned 503; breaker is stuck (413 didn't release the probe slot)")
	}
}

// TestBreakerProbeReleasedOnBodyReadError is the C1 regression for the
// captureHeadAndForward error path. When the inbound body errors mid-
// read, the proxy falls back to transparent forwarding — but the
// fallback also bypasses the response interceptor, so the probe slot
// must still be released. The test sets Failures high enough that the
// body-error alone can't trip the breaker; the only way the second
// request returns 503-by-breaker is if the probe slot leaked.
func TestBreakerProbeReleasedOnBodyReadError(t *testing.T) {
	t.Parallel()

	const recovery = 30 * time.Millisecond
	var allowSuccess atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if !allowSuccess.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		// Failures=10 so a single body-read fallback that may end in
		// a 502 from the upstream-side error handler doesn't trip the
		// breaker on its own. The C1 invariant under test is "probe
		// slot released", not "breaker tolerates body errors".
		Breaker: config.BreakerConfig{Failures: 10, RecoveryWindow: recovery},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Trip the breaker by accumulating 10 × 5xx, then wait for the
	// recovery window so the next admit transitions to half-open.
	driveBreakerToHalfOpen(t, ts.URL, 10, recovery)
	// Flip upstream to healthy so the probe candidate (the body-error
	// request) reaches a happy upstream. Any 5xx that DOES propagate
	// comes from llmtap's own fallback path, not the upstream.
	allowSuccess.Store(true)

	// Probe candidate: chunked body that errors mid-read. The proxy's
	// captureHeadAndForward fails, falls back to transparent forwarding.
	// Whatever status reaches the client, the admission MUST release.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", &erroringBody{
		head: []byte(`{"model":"gpt-4o-mini","messages":[`),
	})
	req.ContentLength = -1 // chunked
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// The body-error path may have cascaded into a 5xx response from
	// llmtap's fallback (the client connection went away mid-body,
	// the proxy can't recover gracefully). That LEGITIMATELY re-trips
	// the breaker via release()'s status-code report — which is the
	// correct behaviour and proves the probe slot was released. The
	// stuck-half-open bug (the C1 fatal flaw) would instead leave
	// probeInFlight=true permanently. To distinguish "tripped again"
	// from "stuck", wait another recovery window and assert the next
	// request is admitted (as a fresh half-open probe).
	time.Sleep(recovery + 10*time.Millisecond)

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp2, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	got, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode == http.StatusServiceUnavailable && strings.Contains(string(got), "circuit breaker") {
		t.Errorf("post-body-error + recovery request returned breaker-503; probe slot leaked (breaker stuck in half-open)")
	}
}

// erroringBody satisfies io.Reader. After the initial head bytes are
// drained, it returns io.ErrUnexpectedEOF to simulate a client that
// hangs up partway through a chunked-encoded body.
type erroringBody struct {
	head []byte
	off  int
	done bool
}

func (e *erroringBody) Read(p []byte) (int, error) {
	if e.done {
		return 0, io.ErrUnexpectedEOF
	}
	if e.off < len(e.head) {
		n := copy(p, e.head[e.off:])
		e.off += n
		return n, nil
	}
	e.done = true
	return 0, io.ErrUnexpectedEOF
}

func (e *erroringBody) Close() error { return nil }
