package proxy_test

import (
	"bytes"
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

// TestUpstreamTransportLiftsPerHostConnCap is the regression for A27.
// The stdlib's http.DefaultTransport caps MaxIdleConnsPerHost at 2,
// which collapses request concurrency against a single upstream to
// "2 + whatever has been freshly dialled but not yet pooled."
// Independent of the per-upstream max_in_flight semaphore, the
// proxy's own Transport must lift this cap to allow real concurrency.
//
// The test parks N requests in the upstream simultaneously and counts
// peak concurrent in-flight upstream observations. With the default
// transport the count would clip near 2; with the tuned transport the
// proxy comfortably hits N.
func TestUpstreamTransportLiftsPerHostConnCap(t *testing.T) {
	t.Parallel()

	const N = 16

	var (
		inFlight atomic.Int32
		peak     atomic.Int32
		release  = make(chan struct{})
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		v := inFlight.Add(1)
		// Track running max.
		for {
			cur := peak.Load()
			if v <= cur || peak.CompareAndSwap(cur, v) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		// No max_in_flight — the proxy's own Transport tuning is
		// what's under test.
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}

	// Wait long enough for all N requests to land on the upstream.
	// If transport tuning is broken, the peak count won't move past
	// ~2 no matter how long we wait.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if peak.Load() >= 10 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := peak.Load(); got < 10 {
		t.Errorf("peak concurrent in-flight upstream requests = %d; want >= 10 (default transport's per-host cap of 2 is the foot-gun)", got)
	}
}

// TestProxyEndToEndStillForwardsAfterTransportTuning is a regression
// guardrail. The transport refactor must not break the non-streaming
// happy path. (The deeper enrichment assertions live elsewhere; this
// test just proves the request still reaches the upstream and the
// response still reaches the client.)
func TestProxyEndToEndStillForwardsAfterTransportTuning(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, got)
	}
	if !strings.Contains(string(got), `"gpt-4o-mini"`) {
		t.Fatalf("client body missing model field: %s", got)
	}
}
