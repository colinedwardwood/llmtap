package proxy_test

import (
	"bytes"
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

// TestProxyRedactsContentEventsByDefault is the end-to-end regression
// for A16: when content.mode is events and content.redact is the
// default-shipped "default", a prompt containing an OpenAI-shaped key
// must never reach the span attribute. Demonstrates the privacy story
// survives a single config edit.
func TestProxyRedactsContentEventsByDefault(t *testing.T) {
	t.Parallel()

	const leakedSecret = "sk-test-AAAAAAAAAAAAAAAAAAAA"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"x",
			"model":"gpt-4o-mini",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}
		}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Content.Mode = config.CaptureEvents
	// Don't override Redact; rely on the documented default ("default").
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, rec, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"my key is ` + leakedSecret + ` please debug"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Walk every recorded span event and assert the secret is absent.
	for _, s := range rec.Ended() {
		for _, ev := range s.Events() {
			for _, a := range ev.Attributes {
				if strings.Contains(a.Value.Emit(), leakedSecret) {
					t.Errorf("span event %q attr %q leaked secret: %q",
						ev.Name, a.Key, a.Value.Emit())
				}
			}
		}
	}
}

// TestProxyRedactOffIsPassthrough asserts that operators who really
// want raw content can still get it — `redact: off` is the explicit
// opt-out.
func TestProxyRedactOffIsPassthrough(t *testing.T) {
	t.Parallel()

	const marker = "marker-string-should-survive"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Content.Mode = config.CaptureEvents
	cfg.Content.Redact = "off"
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	prov, rec, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"` + marker + `"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// At least one span event must carry the marker verbatim.
	found := false
	for _, s := range rec.Ended() {
		for _, ev := range s.Events() {
			for _, a := range ev.Attributes {
				if strings.Contains(a.Value.Emit(), marker) {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("redact=off should pass content verbatim; marker %q not seen on any span event", marker)
	}
}
