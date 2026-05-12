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

// TestErrorBodySnippetSuppressedWhenContentOff is the regression test
// for A2: upstream error responses sometimes echo a prefix of the
// caller's API key (e.g. OpenAI's
// `"Incorrect API key provided: sk-proj-…"`). Today the proxy attaches
// the first 1024 bytes of every 4xx body to the span as
// `http.response.body_snippet`, regardless of content.mode. That
// silently violates the project's flagship privacy default. With
// content.mode = "off", no attribute on the span may contain that
// body verbatim.
func TestErrorBodySnippetSuppressedWhenContentOff(t *testing.T) {
	t.Parallel()

	const leakedSecret = "sk-test-LEAKED-DO-NOT-LOG-THIS-1234567890"
	errorBody := `{"error":{"message":"Incorrect API key provided: ` + leakedSecret + `","type":"invalid_request_error"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, errorBody)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Content.Mode = config.CaptureOff
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

	reqBody := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}

	// The proxy must forward the client-visible error body intact.
	if !strings.Contains(string(body), leakedSecret) {
		t.Fatalf("client did not receive the upstream error body verbatim: %s", body)
	}

	// But no span attribute may carry the leaked secret when
	// content.mode = "off".
	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded")
	}
	for _, s := range spans {
		for _, a := range s.Attributes() {
			if strings.Contains(a.Value.Emit(), leakedSecret) {
				t.Errorf("span attribute %q contains leaked secret: %q", a.Key, a.Value.Emit())
			}
		}
		for _, ev := range s.Events() {
			for _, a := range ev.Attributes {
				if strings.Contains(a.Value.Emit(), leakedSecret) {
					t.Errorf("span event %q attr %q contains leaked secret: %q", ev.Name, a.Key, a.Value.Emit())
				}
			}
		}
	}
}

// TestErrorBodySnippetAttachedWhenContentEvents asserts the snippet
// still flows when the operator has explicitly opted into content
// capture. We must protect privacy by default but not over-redact when
// the operator has chosen otherwise.
func TestErrorBodySnippetAttachedWhenContentEvents(t *testing.T) {
	t.Parallel()

	const marker = "DEBUG-MARKER-IN-ERROR-BODY"
	errorBody := `{"error":{"message":"upstream rejected: ` + marker + `"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errorBody)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Content.Mode = config.CaptureEvents
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

	reqBody := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	spans := rec.Ended()
	var found bool
	for _, s := range spans {
		for _, a := range s.Attributes() {
			if string(a.Key) == "http.response.body_snippet" && strings.Contains(a.Value.AsString(), marker) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected body_snippet to be attached when content.mode=events")
	}
}

// TestProxyForwardsLarge4xxIntact is the regression test for A4: the
// proxy must forward an upstream error body byte-for-byte to the
// client, regardless of how large it is. Previously the body was
// silently truncated at 64 KiB (the snippet-capture limit) and the
// client received only the prefix.
func TestProxyForwardsLarge4xxIntact(t *testing.T) {
	t.Parallel()

	// 200 KiB > old 64 KiB cap.
	bodyText := strings.Repeat("E", 200*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, bodyText)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Content.Mode = config.CaptureEvents
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

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		bytes.NewReader([]byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != len(bodyText) {
		t.Fatalf("client received %d bytes; upstream sent %d", len(got), len(bodyText))
	}

	// Span must have body_snippet of <= 1024 bytes (still a snippet, not
	// the whole body) AND body_size equal to the full upstream size.
	var snippet string
	var size int64 = -1
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			switch string(a.Key) {
			case "http.response.body_snippet":
				snippet = a.Value.AsString()
			case "http.response.body_size":
				size = a.Value.AsInt64()
			}
		}
	}
	if snippet == "" {
		t.Fatal("body_snippet not attached")
	}
	// Snippet is a UTF-8-clean prefix of the body, ≤ 1024 bytes after
	// truncateUTF8 (which may shave a few bytes off the end so a multi-
	// byte rune isn't split, plus the "…" ellipsis when it does truncate).
	if len(snippet) > 1100 {
		t.Errorf("snippet length = %d, want ≤ ~1024", len(snippet))
	}
	if size != int64(len(bodyText)) {
		t.Errorf("body_size = %d, want %d", size, len(bodyText))
	}
}

// TestErrorBodySizeAttachedAlways asserts size metadata (byte count, no
// content) is attached on every 4xx regardless of content.mode, so
// operators have at least a visibility hint that an error body was
// observed.
func TestErrorBodySizeAttachedAlways(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("x", 137)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Content.Mode = config.CaptureOff
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

	reqBody := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var sizeSeen bool
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			if string(a.Key) == "http.response.body_size" && a.Value.AsInt64() == int64(len(body)) {
				sizeSeen = true
			}
		}
	}
	if !sizeSeen {
		t.Errorf("expected http.response.body_size = %d on the span", len(body))
	}
}
