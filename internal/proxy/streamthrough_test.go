package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// TestNonStreamingFirstByteNotDelayed is the regression for A28.
// Before the fix, the non-streaming branch of modify() drained the
// entire response with io.ReadAll before returning, blocking the
// client's first byte for the full upstream latency. The fix wraps
// the body with TeeReader + parseOnClose so bytes stream through
// while a bounded snippet is captured for the parser on Close.
//
// The fake upstream writes the response in two halves separated by
// 100ms. With the old code, the client's first body byte arrives
// AFTER both halves are written and parsed (~100ms+ delta). With the
// fixed code, the first byte arrives roughly when the upstream
// flushes the first half — < 50ms after upstream's first flush.
func TestNonStreamingFirstByteNotDelayed(t *testing.T) {
	t.Parallel()

	// A bespoke net.Listen upstream gives us total control over the
	// write timing: we hijack the connection, flush half the body,
	// sleep, then flush the rest.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	upstreamURL := "http://" + ln.Addr().String()

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, readErr := http.ReadRequest(br); readErr != nil {
					return
				}
				body := `{"id":"x","model":"gpt-4o-mini","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi there"}}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`
				// Half 1: response head + Content-Length + first
				// half of body.
				half := len(body) / 2
				head := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
				if _, writeErr := c.Write([]byte(head)); writeErr != nil {
					return
				}
				if _, writeErr := c.Write([]byte(body[:half])); writeErr != nil {
					return
				}
				// Stall — the proxy must not buffer until close.
				time.Sleep(150 * time.Millisecond)
				if _, writeErr := c.Write([]byte(body[half:])); writeErr != nil {
					return
				}
			}(conn)
		}
	}()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstreamURL, Provider: "openai",
	}}
	prov, rec, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read the first byte and measure the delta. With stream-through,
	// the first byte should arrive shortly after the upstream's first
	// flush — well before the 150ms stall completes.
	first := make([]byte, 1)
	if _, firstErr := io.ReadFull(resp.Body, first); firstErr != nil {
		t.Fatal(firstErr)
	}
	firstByteDelta := time.Since(start)

	// Drain the rest so parseOnClose fires.
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	full := make([]byte, 0, len(first)+len(rest))
	full = append(full, first...)
	full = append(full, rest...)

	if !bytes.Contains(full, []byte(`"hi there"`)) {
		t.Errorf("client body missing expected content: %s", full)
	}

	// Generous threshold (the 150ms stall in the upstream is the
	// "would-be" floor without stream-through). We assert the first
	// byte arrives clearly before the stall completes.
	if firstByteDelta > 100*time.Millisecond {
		t.Errorf("first byte arrived after %v; want < 100ms (non-streaming stream-through is broken)", firstByteDelta)
	}

	// Ensure the existing enrichment path still works: the span must
	// carry gen_ai.usage.input_tokens once Close fires.
	// Allow a brief moment for span.End to land.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(rec.Ended()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded after non-streaming response")
	}
	var found bool
	for _, s := range spans {
		for _, a := range s.Attributes() {
			if string(a.Key) == "gen_ai.usage.input_tokens" {
				found = true
				if a.Value.AsInt64() != 11 {
					t.Errorf("gen_ai.usage.input_tokens = %d; want 11", a.Value.AsInt64())
				}
			}
		}
	}
	if !found {
		t.Error("expected gen_ai.usage.input_tokens attribute on the span; parser never ran")
	}
}
