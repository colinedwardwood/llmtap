package proxy_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// TestUpstreamConcurrencyCapReturns429 is the regression for A12. With
// `max_in_flight = 3` on the openai upstream, firing 4 concurrent
// requests against a slow-responding upstream must yield exactly one
// 429 + 3 successful responses. The 429 carries Retry-After: 1.
func TestUpstreamConcurrencyCapReturns429(t *testing.T) {
	t.Parallel()

	const cap = 3
	const overshoot = 1

	// Block upstream long enough that all 4 client goroutines are
	// concurrently in-flight against the proxy before the first one
	// returns. The semaphore must serialise the 4th to 429 instead of
	// admitting it.
	release := make(chan struct{})
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()
	defer close(release)

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		MaxInFlight: cap,
	}}

	prov, _, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	var (
		statuses    = make([]int, cap+overshoot)
		retryAfters = make([]string, cap+overshoot)
		wg          sync.WaitGroup
	)
	// Stagger the first wave so the proxy gets to admit them into the
	// semaphore before the overshoot arrives. Without this, all 4 hit
	// the TryAcquire at once and which gets 429 is racy.
	for i := 0; i < cap; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(),
				http.MethodPost, ts.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			statuses[i] = resp.StatusCode
			retryAfters[i] = resp.Header.Get("Retry-After")
			_ = resp.Body.Close()
		}(i)
	}
	// Wait for the first wave to be parked in the upstream.
	for upstreamHits.Load() < int32(cap) {
		time.Sleep(5 * time.Millisecond)
	}
	// Fire the overshoot — should hit a full semaphore.
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequestWithContext(context.Background(),
			http.MethodPost, ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("overshoot client: %v", err)
			return
		}
		statuses[cap] = resp.StatusCode
		retryAfters[cap] = resp.Header.Get("Retry-After")
		_ = resp.Body.Close()
	}()

	// Drain. Give the overshoot a moment to land before releasing the
	// first wave; otherwise it might enter as one of them returns.
	time.Sleep(50 * time.Millisecond)
	// Release the slow upstream. First-wave goroutines unblock and
	// return 200.
	for i := 0; i < cap; i++ {
		release <- struct{}{}
	}
	wg.Wait()

	got200, got429 := 0, 0
	var retryAfter429 string
	for i, s := range statuses {
		switch s {
		case http.StatusOK:
			got200++
		case http.StatusTooManyRequests:
			got429++
			retryAfter429 = retryAfters[i]
		default:
			t.Errorf("client %d got unexpected status %d", i, s)
		}
	}
	if got200 != cap {
		t.Errorf("got %d × 200, want %d", got200, cap)
	}
	if got429 != overshoot {
		t.Errorf("got %d × 429, want %d", got429, overshoot)
	}
	if retryAfter429 != "1" {
		t.Errorf("Retry-After on 429 = %q, want %q", retryAfter429, "1")
	}
	// Upstream must have been hit exactly cap times — never the
	// overshoot.
	if hits := upstreamHits.Load(); hits != int32(cap) {
		t.Errorf("upstream hits = %d, want %d", hits, cap)
	}
}

// TestUpstreamConcurrencyUnboundedByDefault: MaxInFlight=0 means no cap.
func TestUpstreamConcurrencyUnboundedByDefault(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		// MaxInFlight: 0 (unlimited)
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(),
				http.MethodPost, ts.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 (no cap configured)", resp.StatusCode)
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	if hits := upstreamHits.Load(); hits != n {
		t.Errorf("upstream hits = %d, want %d", hits, n)
	}
}
