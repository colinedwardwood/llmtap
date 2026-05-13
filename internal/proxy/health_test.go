package proxy_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/auth"
	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// TestHealthzAlwaysOK: GET /healthz returns 200 + "ok", regardless of
// auth gate, upstream state, or anything else. The endpoint is the
// minimum-viable signal that the process is up.
func TestHealthzAlwaysOK(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	// Auth gate is on — /healthz must bypass it. The exact token
	// isn't load-bearing because the test never presents it; the
	// assertion is that /healthz returns 200 WITHOUT a token.
	hashed, err := auth.Hash("test-token")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.Tokens = []string{hashed}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := strings.TrimSpace(string(body)); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Errorf("upstream hits = %d, want 0 (/healthz must not reach upstream)", got)
	}
}

// TestReadyzReturnsExpectedStatus: when the readiness callback
// returns true, /readyz returns 200 + "ready". When it returns
// false, /readyz returns 503 + Retry-After: 1.
func TestReadyzReturnsExpectedStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		ready     func() bool
		wantCode  int
		wantBody  string
		wantRetry string
	}{
		{
			name:      "ready=true",
			ready:     func() bool { return true },
			wantCode:  http.StatusOK,
			wantBody:  "ready",
			wantRetry: "",
		},
		{
			name:      "ready=false",
			ready:     func() bool { return false },
			wantCode:  http.StatusServiceUnavailable,
			wantBody:  "not ready",
			wantRetry: "1",
		},
		{
			name:      "ready=nil treated as always-ready",
			ready:     nil,
			wantCode:  http.StatusOK,
			wantBody:  "ready",
			wantRetry: "",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()

			cfg := config.Default()
			cfg.Upstreams = []config.Upstream{{
				Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
			}}
			prov := fakeProv(t)
			prov.Ready = tc.ready
			h, err := proxy.New(cfg, provider.BuiltIn(), prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(h)
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/readyz")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantCode {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
			if got := strings.TrimSpace(string(body)); got != tc.wantBody {
				t.Errorf("body = %q, want %q", got, tc.wantBody)
			}
			if got := resp.Header.Get("Retry-After"); got != tc.wantRetry {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
			}
		})
	}
}

// TestUpstreamPrefixCannotCollideWithHealthz: an upstream whose prefix
// is /healthz or /readyz (or a path under them) must fail config
// validation. The proxy can't serve the probe endpoints if an
// upstream claims their address.
func TestUpstreamPrefixCannotCollideWithHealthz(t *testing.T) {
	t.Parallel()

	cases := []string{"/healthz", "/readyz", "/healthz/sub", "/readyz/x"}
	for _, prefix := range cases {
		prefix := prefix
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Upstreams = []config.Upstream{{
				Name: "rogue", Prefix: prefix, Target: "https://example.com", Provider: "openai",
			}}
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() accepted reserved prefix %q; want error", prefix)
			}
		})
	}
}

// TestReadyzDoesNotReachUpstream: even when the proxy is configured
// with a /readyz-adjacent upstream prefix (e.g. /readyz-internal),
// GET /readyz answers locally without contacting the upstream.
func TestReadyzDoesNotReachUpstream(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
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

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if upstreamHits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0 (/readyz must not proxy)", upstreamHits.Load())
	}
}
