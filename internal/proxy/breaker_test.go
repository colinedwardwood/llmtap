package proxy_test

import (
	"bytes"
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

// TestBreakerOpensAfterConsecutiveFailures: with breaker.failures=3,
// after three consecutive 5xx responses the breaker trips. The fourth
// request must short-circuit with 503 + Retry-After before reaching
// the upstream.
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"upstream"}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{
			Failures:       3,
			Window:         5 * time.Second,
			RecoveryWindow: 2 * time.Second,
		},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	// First three requests: 500 from upstream, breaker closed -> open
	// on the third's report.
	for i := 0; i < 3; i++ {
		resp, postErr := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if postErr != nil {
			t.Fatalf("req %d: %v", i, postErr)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("req %d: status = %d, want 500", i, resp.StatusCode)
		}
	}
	if got := upstreamHits.Load(); got != 3 {
		t.Fatalf("upstream hits after warmup = %d, want 3", got)
	}

	// Fourth request: breaker open, expect 503 + Retry-After, NO
	// upstream contact.
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("4th req status = %d, want 503 (breaker open)", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("4th req missing Retry-After header on 503")
	}
	if got := upstreamHits.Load(); got != 3 {
		t.Errorf("upstream hits after breaker open = %d, want 3 (breaker should short-circuit)", got)
	}
}

// TestBreakerHalfOpenAdmitsOneProbe: once RecoveryWindow elapses, the
// breaker admits one probe — but a SECOND simultaneous request while
// the probe is in flight must still 503.
func TestBreakerHalfOpenAdmitsOneProbe(t *testing.T) {
	t.Parallel()

	// Use a short recovery window so the test is fast.
	const recovery = 50 * time.Millisecond

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		// Stay slow so the probe is in-flight when the second
		// request arrives.
		time.Sleep(80 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{
			Failures:       2,
			RecoveryWindow: recovery,
		},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	// Trip the breaker.
	for i := 0; i < 2; i++ {
		resp, postErr := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if postErr != nil {
			t.Fatal(postErr)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if upstreamHits.Load() != 2 {
		t.Fatalf("warmup hits = %d, want 2", upstreamHits.Load())
	}

	// Wait for RecoveryWindow to expire.
	time.Sleep(recovery + 5*time.Millisecond)

	// Fire two concurrent requests. One should be admitted as a
	// probe; the other must reject with 503.
	type result struct {
		status int
		err    error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				results <- result{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			results <- result{status: resp.StatusCode}
		}()
	}

	var got503, gotOther int
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		switch r.status {
		case http.StatusServiceUnavailable:
			got503++
		default:
			gotOther++
		}
	}
	if got503 != 1 {
		t.Errorf("half-open: got %d × 503, want exactly 1 (only one probe permitted)", got503)
	}
	if gotOther != 1 {
		t.Errorf("half-open: got %d non-503 responses, want exactly 1 (the probe)", gotOther)
	}
	if hits := upstreamHits.Load(); hits != 3 {
		t.Errorf("half-open upstream hits = %d, want 3 (2 warmup + 1 probe)", hits)
	}
}

// TestBreakerClosesOnProbeSuccess: after open → half-open → probe
// succeeds with a 200, the breaker closes and subsequent requests
// flow through normally.
func TestBreakerClosesOnProbeSuccess(t *testing.T) {
	t.Parallel()

	const recovery = 50 * time.Millisecond

	// Upstream is configurable mid-test via an atomic.
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
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{
			Failures:       2,
			RecoveryWindow: recovery,
		},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	// Trip the breaker.
	for i := 0; i < 2; i++ {
		resp, _ := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// Flip upstream to healthy and wait for recovery.
	fail.Store(false)
	time.Sleep(recovery + 5*time.Millisecond)

	// Probe request: should succeed and close the breaker.
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d, want 200", resp.StatusCode)
	}

	// Subsequent requests must flow through immediately — breaker
	// is closed.
	for i := 0; i < 3; i++ {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post-recovery req %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("post-recovery req %d status = %d, want 200 (breaker should be closed)", i, resp.StatusCode)
		}
	}
}

// TestBreakerDisabledByDefault: with Failures=0 the breaker never
// short-circuits, even under a sustained 5xx storm.
func TestBreakerDisabledByDefault(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	for i := 0; i < 5; i++ {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("req %d status = %d, want 500 (no breaker)", i, resp.StatusCode)
		}
	}
	if hits.Load() != 5 {
		t.Errorf("upstream hits = %d, want 5 (breaker disabled, all 5 should pass through)", hits.Load())
	}
	// Defensive check: keep strings import used.
	_ = strings.Builder{}
}
