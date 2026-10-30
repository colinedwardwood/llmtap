package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
)

// TestClientDisconnectDoesNotTripBreaker is the regression test for D7
// (review finding R4): client hangups reached the ErrorHandler, which
// wrote an unconditional 502, which the breaker counted as an upstream
// failure. A client-side deploy cancelling a handful of in-flight
// requests opened the circuit against a perfectly healthy upstream.
func TestClientDisconnectDoesNotTripBreaker(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hang") == "1" {
			// Drain the body first: net/http only watches the
			// connection (and cancels the handler context on peer
			// close) once the request body has been consumed.
			_, _ = io.Copy(io.Discard, r.Body)
			// Hold until the proxy's outbound request is cancelled by
			// the client disconnect.
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 2, RecoveryWindow: time.Hour},
	}}
	prov, _ := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`

	// Three aborted requests — one more than the trip threshold.
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			ts.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hang", "1")
		resp, doErr := http.DefaultClient.Do(req)
		if doErr == nil {
			_ = resp.Body.Close()
			t.Fatalf("expected client-side cancellation, got status %d", resp.StatusCode)
		}
		cancel()
	}

	// Give the server-side release() defers a beat to run.
	time.Sleep(100 * time.Millisecond)

	// A healthy request must pass: the breaker must still be closed.
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("breaker tripped on client disconnects (503): %s", respBody)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, respBody)
	}
}

// TestMidStreamBodyCapDoesNotTripBreaker pins the second half of D7: a
// request body that blows the hard cap AFTER forwarding started (chunked,
// no Content-Length pre-check possible) is a client-side fault. It must
// surface as 413, not 502, and must not count against the breaker.
func TestMidStreamBodyCapDoesNotTripBreaker(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain whatever arrives so the proxy's outbound write makes
		// progress until the cap aborts it.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","model":"gpt-4o-mini","choices":[]}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	// Cap above the 1 MiB enrichment buffer so the overflow is only
	// detectable mid-stream, by capCountingReader.
	cfg.Request.MaxBodyBytes = 2 * 1024 * 1024
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 1, RecoveryWindow: time.Hour},
	}}
	prov, _ := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 3 MiB with the length hidden, so the client sends chunked and the
	// ContentLength pre-check can't reject it up front.
	huge := bytes.Repeat([]byte("a"), 3*1024*1024)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		io.MultiReader(bytes.NewReader(huge)))
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := http.DefaultClient.Do(req)
	if doErr == nil {
		// If a response made it back it must be the honest 413, never
		// the upstream-blaming 502.
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("mid-stream cap status = %d, want 413", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	time.Sleep(100 * time.Millisecond)

	// Breaker (failures=1) must not have tripped.
	resp2, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("breaker tripped on mid-stream body-cap abort (503): %s", respBody)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp2.StatusCode, respBody)
	}
}
