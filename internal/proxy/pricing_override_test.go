package proxy_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// TestProxyEmitsOverriddenCost is the end-to-end regression for A7.
// An operator-supplied pricing.path must actually change the
// gen_ai.cost.usd attribute the proxy records — not just live in the
// pricing package's unit tests.
func TestProxyEmitsOverriddenCost(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"x",
			"model":"gpt-4o-mini",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
			"usage":{"prompt_tokens":1000000,"completion_tokens":0}
		}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	pricesPath := filepath.Join(dir, "prices.yaml")
	// Override rate: 0.99 USD per million input tokens. With
	// prompt_tokens=1_000_000 the recorded cost should be exactly 0.99,
	// versus the built-in rate of 0.15 for gpt-4o-mini.
	if err := os.WriteFile(pricesPath, []byte(`
openai:
  gpt-4o-mini:
    input_usd_per_mtok: 0.99
    output_usd_per_mtok: 0.99
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	cfg.Pricing.Path = pricesPath
	prov, rec, _ := realMeterProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		bytes.NewReader([]byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var cost float64 = -1
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			if string(a.Key) == "gen_ai.cost.usd" {
				cost = a.Value.AsFloat64()
			}
		}
	}
	if cost < 0 {
		t.Fatal("gen_ai.cost.usd not recorded on span")
	}
	// Cost = 1_000_000 input tokens * 0.99 USD/M = 0.99.
	const want = 0.99
	if d := cost - want; d > 0.001 || d < -0.001 {
		t.Errorf("gen_ai.cost.usd = %v, want %v (override didn't engage)", cost, want)
	}
}

// TestProxyNewFailsOnMissingPricingFileFailClosed: A misconfigured
// pricing path must fail loud when fail_open=false. Avoids the
// silent-degradation footgun the operator is explicitly opting out of.
func TestProxyNewFailsOnMissingPricingFileFailClosed(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Pricing.Path = "/nonexistent/llmtap-prices.yaml"
	cfg.Pricing.FailOpen = false
	prov, _, _ := realMeterProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := proxy.New(cfg, provider.BuiltIn(), prov, logger); err == nil {
		t.Fatal("expected proxy.New to refuse a missing pricing file with fail_open=false")
	}
}
