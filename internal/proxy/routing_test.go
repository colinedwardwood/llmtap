package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
)

// TestStripPrefixForwardsUpstreamPath is the regression test for the
// broken-by-default Anthropic route (D5). The default config mounts
// Anthropic at the routing-only prefix /anthropic, but ProxyRequest.SetURL
// only JOINS paths — before the fix the upstream received
// /anthropic/v1/messages and 404'd every documented request. With
// strip_prefix: true the upstream must see /v1/messages.
func TestStripPrefixForwardsUpstreamPath(t *testing.T) {
	t.Parallel()

	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022",
			"content":[{"type":"text","text":"hi"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":2}
		}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "anthropic", Prefix: "/anthropic", Target: upstream.URL,
		Provider: "anthropic", StripPrefix: true,
	}}
	prov, rec := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := `{"model":"claude-3-5-sonnet-20241022","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(ts.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, respBody)
	}
	if upstreamPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want %q (routing prefix not stripped)", upstreamPath, "/v1/messages")
	}

	// Enrichment must still run against the (prefixed) inbound path.
	var found bool
	for _, s := range rec.Ended() {
		if strings.HasPrefix(s.Name(), "chat ") {
			found = true
		}
	}
	if !found {
		t.Fatal("stripped route lost GenAI enrichment: no chat span recorded")
	}
}

// TestNoStripPrefixJoinsPath pins the OpenAI-shaped case: when the
// mount prefix IS the upstream's real base path (/v1), strip_prefix
// stays false and the joined path forwards unchanged.
func TestNoStripPrefixJoinsPath(t *testing.T) {
	t.Parallel()

	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, _ := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if upstreamPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", upstreamPath)
	}
}

// TestDotSegmentPathRoutesCanonically pins D6: routing, enrichment, and
// forwarding must agree on one path identity. Before the fix,
// /v1/../anthropic/v1/messages byte-prefix-matched the /v1 upstream
// while the upstream's own normalization would have served the
// anthropic path — a parser differential.
func TestDotSegmentPathRoutesCanonically(t *testing.T) {
	t.Parallel()

	var openaiHits, anthropicHits int
	var anthropicPath string
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		openaiHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer openaiUp.Close()
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHits++
		anthropicPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer anthropicUp.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{
		{Name: "openai", Prefix: "/v1", Target: openaiUp.URL, Provider: "openai"},
		{Name: "anthropic", Prefix: "/anthropic", Target: anthropicUp.URL, Provider: "anthropic", StripPrefix: true},
	}
	prov, _ := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/../anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-3-5-sonnet-20241022","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultTransport.RoundTrip(req) // Transport: no client-side URL cleaning
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if openaiHits != 0 {
		t.Fatalf("dot-segment path leaked to the openai upstream (%d hits)", openaiHits)
	}
	if anthropicHits != 1 {
		t.Fatalf("anthropic upstream hits = %d, want 1", anthropicHits)
	}
	if anthropicPath != "/v1/messages" {
		t.Fatalf("anthropic upstream path = %q, want /v1/messages", anthropicPath)
	}
}

// TestSiblingPathDoesNotMatchPrefix pins segment-aware matching:
// /v1foo must 404, not route into the /v1 upstream.
func TestSiblingPathDoesNotMatchPrefix(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, _ := fakeProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1foo/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1foo/... = %d, want 404", resp.StatusCode)
	}
}
