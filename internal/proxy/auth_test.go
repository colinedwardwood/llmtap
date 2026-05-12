package proxy_test

import (
	"context"
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

// proxyWithAuth builds an httptest-backed proxy with the supplied
// allow-list and returns the proxy URL, the upstream counter, and a
// teardown.
func proxyWithAuth(t *testing.T, tokens []string, header string) (string, *atomic.Int32, func()) {
	t.Helper()
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Defensive assertion: the proxy must strip the auth header
		// before forwarding. Catches a future regression where the
		// client's bearer token leaks to the upstream LLM.
		if r.Header.Get("X-LLMTAP-Token") != "" {
			t.Errorf("auth header leaked to upstream: %q", r.Header.Get("X-LLMTAP-Token"))
		}
		if h := header; h != "" && r.Header.Get(h) != "" {
			t.Errorf("auth header %q leaked to upstream: %q", h, r.Header.Get(h))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	cfg.Auth.Tokens = tokens
	cfg.Auth.Header = header

	prov, _, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	return ts.URL, &hits, func() { ts.Close(); upstream.Close() }
}

func TestProxyRejects401WithoutToken(t *testing.T) {
	t.Parallel()
	hash, _ := auth.Hash("secret-A")
	url, hits, teardown := proxyWithAuth(t, []string{hash}, "")
	defer teardown()

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if h := hits.Load(); h != 0 {
		t.Errorf("upstream hit %d times; want 0", h)
	}
}

func TestProxyRejects401WithWrongToken(t *testing.T) {
	t.Parallel()
	hash, _ := auth.Hash("real-secret")
	url, hits, teardown := proxyWithAuth(t, []string{hash}, "")
	defer teardown()

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req.Header.Set("X-LLMTAP-Token", "wrong-guess")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if h := hits.Load(); h != 0 {
		t.Errorf("upstream hit %d times; want 0", h)
	}
}

func TestProxyAcceptsCorrectToken(t *testing.T) {
	t.Parallel()
	hash, _ := auth.Hash("ok-secret")
	url, hits, teardown := proxyWithAuth(t, []string{hash}, "")
	defer teardown()

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req.Header.Set("X-LLMTAP-Token", "Bearer ok-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if h := hits.Load(); h != 1 {
		t.Errorf("upstream hit %d times; want 1", h)
	}
}

func TestProxyAcceptsBareToken(t *testing.T) {
	t.Parallel()
	hash, _ := auth.Hash("ok-secret")
	url, hits, teardown := proxyWithAuth(t, []string{hash}, "")
	defer teardown()

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	// No "Bearer " prefix — proxy should accept either form.
	req.Header.Set("X-LLMTAP-Token", "ok-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (bare-token form should work too)", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream not hit for valid bare-token request")
	}
}

func TestProxyNoAuthWhenTokensEmpty(t *testing.T) {
	t.Parallel()
	url, hits, teardown := proxyWithAuth(t, nil, "")
	defer teardown()

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (auth disabled when no tokens)", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream not reached when auth is disabled")
	}
}

func TestProxyCustomAuthHeader(t *testing.T) {
	t.Parallel()
	hash, _ := auth.Hash("ok-secret")
	url, hits, teardown := proxyWithAuth(t, []string{hash}, "X-My-Token")
	defer teardown()

	// Wrong header name — must 401.
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req.Header.Set("X-LLMTAP-Token", "ok-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("default header should NOT auth when custom header configured: %d", resp.StatusCode)
	}

	// Correct custom header — must 200.
	req2, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req2.Header.Set("X-My-Token", "ok-secret")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("custom header should auth: %d", resp2.StatusCode)
	}
	if hits.Load() != 1 {
		t.Errorf("expected exactly one upstream hit, got %d", hits.Load())
	}
}
