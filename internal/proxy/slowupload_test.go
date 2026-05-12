package proxy_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// slowReader emits one byte per delay. Used to simulate a slowloris-
// style body upload at the application layer.
type slowReader struct {
	remaining int
	delay     time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.remaining <= 0 {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	p[0] = 'a'
	s.remaining--
	return 1, nil
}

func (s *slowReader) Close() error { return nil }

// configureProxyServer builds the proxy.Handler and wraps it in an
// httptest.NewUnstartedServer so we can set http.Server.ReadTimeout
// from cfg — that's the knob the A11 fix exercises.
func configureProxyServer(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(h)
	ts.Config.ReadTimeout = cfg.HTTP.BodyReadTimeout
	ts.Config.ReadHeaderTimeout = cfg.HTTP.ReadHeaderTimeout
	ts.Start()
	return ts
}

// TestSlowUploadIsTerminated is the regression test for A11. With a
// 300 ms body-read deadline, a body that takes many seconds to deliver
// must be aborted by the proxy and zero upstream traffic must reach
// the upstream. Confirms the proxy does not pin the request goroutine
// for the entire client-controlled upload duration.
//
// Implementation detail: net/http's Server.ReadTimeout fires at the
// TCP-conn level — the connection is closed mid-read, so the client
// sees a transport error rather than a graceful 408. The contract is
// "the request is terminated", not "a specific status code is
// returned" — slowloris protection is about goroutine pinning, not
// error-channel UX.
func TestSlowUploadIsTerminated(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	cfg.HTTP.BodyReadTimeout = 300 * time.Millisecond

	ts := configureProxyServer(t, cfg)
	defer ts.Close()

	// 5 KB at 50 ms/byte = 250 s. Deadline is 300 ms.
	body := &slowReader{remaining: 5 * 1024, delay: 50 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.URL+"/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	// ContentLength = -1 forces chunked transfer.
	req.ContentLength = -1

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)

	// Either a transport-level error (conn closed mid-write) or a
	// short response from the server — both prove the proxy aborted.
	if err == nil {
		_ = resp.Body.Close()
		// A graceful 408 from net/http when ReadTimeout fires after
		// the request line has been sent. Still proof of termination.
		if resp.StatusCode == http.StatusOK {
			t.Errorf("status = 200 — proxy let the slow upload through")
		}
	}
	if upstreamHits.Load() != 0 {
		t.Errorf("upstream was hit %d times; want 0 on body-read timeout", upstreamHits.Load())
	}
	// Must NOT have waited for the full 5 KB upload (~250 s).
	if maxOK := 2 * time.Second; elapsed > maxOK {
		t.Errorf("proxy held the request for %v; want <= %v (deadline was 300 ms)", elapsed, maxOK)
	}
}

// TestNoBodyReadTimeoutWhenUnset asserts that BodyReadTimeout=0 keeps
// the pre-A11 behaviour: no deadline, slow uploads complete.
func TestNoBodyReadTimeoutWhenUnset(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	cfg.HTTP.BodyReadTimeout = 0 // disabled

	ts := configureProxyServer(t, cfg)
	defer ts.Close()

	// Tiny slow body — enough to prove the deadline isn't firing,
	// but small enough to not slow the suite.
	body := &slowReader{remaining: 8, delay: 30 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.URL+"/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (deadline disabled should allow slow upload)", upstreamHits.Load())
	}
}
