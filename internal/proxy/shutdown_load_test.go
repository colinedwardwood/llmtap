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
	"sync"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// TestShutdownUnderSustainedStreamingLoad is the regression test for D8
// (review finding R6): Server.Run used to drain activeStreams BEFORE
// http.Server.Shutdown closed the listener. Under sustained streaming
// traffic new streams kept arriving, the counter never reached zero,
// the entire shutdown budget burned in WaitForStreams, and Shutdown
// then ran with an expired context — hard-killing everything. With the
// fixed ordering (listener closes first, drain runs concurrently),
// shutdown under load completes promptly.
func TestShutdownUnderSustainedStreamingLoad(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, `data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(15 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

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
	cfg.HTTP.ShutdownTimeout = 10 * time.Second

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
	go func() { runReturned <- srv.Run(runCtx) }()

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

	// Sustained streaming load: several clients looping so there is
	// almost always at least one active stream on the proxy.
	reqBody, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, reqErr := http.Post("http://"+addr+"/v1/chat/completions",
					"application/json", bytes.NewReader(reqBody))
				if reqErr != nil {
					// Listener closed — shutdown in progress.
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}

	// Let the load establish, then request shutdown mid-traffic.
	time.Sleep(300 * time.Millisecond)
	cancelRun()

	select {
	case err := <-runReturned:
		if err != nil {
			t.Fatalf("Server.Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Server.Run did not return within 5s under sustained load — shutdown ordering regression")
	}

	close(stop)
	wg.Wait()
}
