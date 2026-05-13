package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// fakeProvidersForShutdown mirrors fakeProviders from proxy_test.go but
// without depending on its internal-package access — this test file lives
// in the `proxy_test` package because it needs the public Server API.
func fakeProvidersForShutdown(t *testing.T) telemetry.Providers {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	meter := noop.NewMeterProvider().Meter("noop")
	tokens, _ := meter.Int64Histogram("gen_ai.client.token.usage")
	dur, _ := meter.Float64Histogram("gen_ai.client.operation.duration")
	ttft, _ := meter.Float64Histogram("gen_ai.client.time_to_first_token")
	cost, _ := meter.Float64Counter("gen_ai.client.cost.usd")
	return telemetry.Providers{
		Tracer: tp.Tracer("test"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage:        tokens,
			OperationDuration: dur,
			TimeToFirstToken:  ttft,
			CostUSD:           cost,
		},
		Shutdown: func(context.Context) error { return nil },
	}
}

// TestShutdownWaitsForActiveStreams is the regression test for A20:
// Server.Run must not return from the shutdown branch until in-flight SSE
// streams have actually released. Before the fix, Shutdown returned as
// soon as the request handler goroutine exited — but the streaming wrapper
// runs finalize in an onClose callback that fires AFTER the handler
// returns. Metric emission and span End for the dominant streaming case
// were getting raced against process exit.
func TestShutdownWaitsForActiveStreams(t *testing.T) {
	t.Parallel()

	// release is closed when the test wants the upstream to finish the
	// stream and let onClose fire. Until then, the upstream hangs
	// mid-stream — the proxy has one active stream pinned.
	release := make(chan struct{})
	streamStarted := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Emit one chunk so the proxy commits to the streaming path and
		// the activeStreams counter is observably non-zero.
		_, _ = io.WriteString(w, `data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		close(streamStarted)
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	// Pick a free local port so Server.Run binds successfully.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := config.Default()
	cfg.Listen = addr
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	// Give the drain window enough budget to comfortably outlast our
	// test's release latency. We assert Run-blocks-until-release, not
	// Run-exits-fast, so this needs to be > the time we hold release.
	cfg.HTTP.ShutdownTimeout = 5 * time.Second

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov := fakeProvidersForShutdown(t)

	handler, err := proxy.New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := proxy.NewServer(cfg, handler, logger)
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	runReturned := make(chan error, 1)
	go func() {
		runReturned <- srv.Run(runCtx)
	}()

	// Wait for the listener to actually be ready. We poll the addr.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s: %v", addr, dialErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Fire a streaming request through the proxy. Drain only what we
	// need to confirm the upstream actually started; leave the body
	// open so activeStreams stays positive on the proxy side.
	reqBody, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	clientResp := make(chan *http.Response, 1)
	clientErr := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		resp, postErr := http.DefaultClient.Do(req)
		if postErr != nil {
			clientErr <- postErr
			return
		}
		// Read one chunk to push past the response-headers boundary, then
		// hang on the body — keeps the stream parser side alive.
		buf := make([]byte, 256)
		_, _ = resp.Body.Read(buf)
		clientResp <- resp
	}()

	select {
	case <-streamStarted:
	case err := <-clientErr:
		t.Fatalf("client request failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never emitted first event")
	}

	// Trigger shutdown. Run should block until release is closed and
	// the stream onClose has fired.
	cancelRun()

	// First, confirm Run is STILL running while the stream is pinned.
	select {
	case err := <-runReturned:
		t.Fatalf("Server.Run returned %v while stream still active — A20 regression", err)
	case <-time.After(150 * time.Millisecond):
		// Good — Run is blocked waiting on WaitForStreams.
	}

	// Release the upstream so onClose fires and activeStreams drops to 0.
	close(release)
	// Drain client body so the stream wrapper sees EOF.
	go func() {
		select {
		case resp := <-clientResp:
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		case <-time.After(2 * time.Second):
		}
	}()

	select {
	case err := <-runReturned:
		if err != nil {
			t.Fatalf("Server.Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Server.Run did not return after stream released")
	}
}
