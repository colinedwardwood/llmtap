package proxy_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// TestProxyGzippedResponseStillCountsTokens is the regression test for A6.
//
// When a client sets Accept-Encoding: gzip, http.Transport's
// transparent decompression backs off — it only decodes responses it
// asked for itself. The proxy then sees gzip-encoded bytes in modify,
// json.Unmarshal fails silently, and every recorded span shows
// input_tokens = 0, output_tokens = 0, cost = $0. A FinOps disaster
// dressed up as a parser quirk.
//
// With A6 fixed (Accept-Encoding stripped at the proxy boundary), the
// Transport requests + decompresses gzip on llmtap's behalf, and
// parseResponseJSON sees plaintext.
func TestProxyGzippedResponseStillCountsTokens(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Always serve gzipped JSON, regardless of Accept-Encoding,
		// to keep the test deterministic. Production upstreams behave
		// similarly for clients that requested compression.
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = io.WriteString(gz, `{
			"id":"chatcmpl-test",
			"model":"gpt-4o-mini-2024-07-18",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
			"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}
		}`)
		_ = gz.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", "")
		_, _ = w.Write(buf.Bytes())
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, rec, _ := realMeterProviders(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	// Force the failure mode: explicit Accept-Encoding suppresses the
	// transparent gzip decoding that Transport would otherwise inject.
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")

	// Use a custom Transport with DisableCompression=true so the test
	// client itself doesn't decode anything we read for assertions.
	client := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}

	// Span must carry parsed token usage. Without the fix, both are 0.
	var inputTokens int64
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			if string(a.Key) == "gen_ai.usage.input_tokens" {
				inputTokens = a.Value.AsInt64()
			}
		}
	}
	if inputTokens <= 0 {
		// Dump for diagnosis on failure.
		for _, s := range rec.Ended() {
			t.Logf("span %q attrs:", s.Name())
			for _, a := range s.Attributes() {
				t.Logf("  %s = %s", a.Key, a.Value.Emit())
			}
		}
		t.Fatalf("gen_ai.usage.input_tokens = %d, want > 0 (gzip response was not decoded for parsing)", inputTokens)
	}
}

// TestProxyStripsAcceptEncodingFromOutbound asserts the mechanism A6
// relies on: the outbound request to the upstream must not carry the
// client's Accept-Encoding header verbatim. Without this, the Go
// Transport's transparent gzip decoding stays disabled.
func TestProxyStripsAcceptEncodingFromOutbound(t *testing.T) {
	t.Parallel()

	gotAccept := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
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

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req.Header.Set("Accept-Encoding", "gzip, br")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	upstreamSaw := <-gotAccept
	// We expect Go's Transport to inject its own Accept-Encoding: gzip
	// (so it can auto-decode), but the client's mixed "gzip, br" must
	// not pass through unchanged.
	if upstreamSaw == "gzip, br" {
		t.Errorf("upstream saw client's Accept-Encoding verbatim = %q (proxy did not strip it)", upstreamSaw)
	}
}
