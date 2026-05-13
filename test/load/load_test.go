//go:build loadtest

// Package load is a Go-native load-test harness for llmtap. It fires a
// configurable RPS at the proxy against an in-process fake upstream
// and asserts:
//
//   - p99 added latency (end-to-end minus fake-upstream baseline) is
//     under the budget,
//   - zero 5xx responses,
//   - goroutine count returns close to the baseline after teardown
//     (a 200 % allowance covers transient stdlib goroutines from the
//     metric / SBOM / batch processors winding down).
//
// Run via `make load-test`. Skipped under -short.
//
// Tunable via environment:
//
//	LOAD_RPS         requests/second (default 200)
//	LOAD_DURATION    test duration   (default 10s)
//	LOAD_P99_BUDGET  max allowed p99 added latency (default 5ms)
package load_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func envDuration(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("bad %s=%q: %v", key, v, err)
	}
	return d
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("bad %s=%q: %v", key, v, err)
	}
	return n
}

func fakeProv() telemetry.Providers {
	tp := sdktrace.NewTracerProvider()
	meter := noop.NewMeterProvider().Meter("noop")
	tokens, _ := meter.Int64Histogram("tu")
	dur, _ := meter.Float64Histogram("du")
	ttft, _ := meter.Float64Histogram("ttft")
	cost, _ := meter.Float64Histogram("cost")
	costTotal, _ := meter.Float64Counter("cost.total")
	return telemetry.Providers{
		Tracer: tp.Tracer("load"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage:        tokens,
			OperationDuration: dur,
			TimeToFirstToken:  ttft,
			CostUSD:           cost,
			CostUSDTotal:      costTotal,
		},
		Shutdown: func(context.Context) error { return nil },
	}
}

// TestLoad fires LOAD_RPS req/s at the proxy for LOAD_DURATION and
// asserts the contract above. Skipped under -short.
func TestLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in -short mode")
	}

	rps := envInt(t, "LOAD_RPS", 200)
	duration := envDuration(t, "LOAD_DURATION", 10*time.Second)
	p99Budget := envDuration(t, "LOAD_P99_BUDGET", 5*time.Millisecond)

	const cannedResponse = `{
		"id":"x",
		"model":"gpt-4o-mini",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
		"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}
	}`

	// Fake upstream: emit the canned response immediately. The
	// "added latency" we measure is the proxy's contribution alone —
	// the test asserts the proxy adds less than p99Budget.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, cannedResponse)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}

	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Sample the fake-upstream baseline (loop-back-to-loop-back direct,
	// no proxy in the path) so we can attribute *added* latency.
	baseline := measureBaseline(t, upstream.URL+"/v1/chat/completions")

	// Pre-warm the client pool so first-shot dialer latency doesn't
	// pollute the sample.
	for i := 0; i < 16; i++ {
		_, _ = post(ts.URL+"/v1/chat/completions", requestBody())
	}

	goroutinesBefore := runtime.NumGoroutine()

	// Fire LOAD_RPS req/s for LOAD_DURATION via a token-bucket
	// ticker. Bounded worker pool keeps in-flight count predictable
	// regardless of the proxy's response latency.
	const workers = 64
	tick := time.NewTicker(time.Second / time.Duration(rps))
	defer tick.Stop()
	stop := time.After(duration)

	latencies := make(chan time.Duration, rps*int(duration/time.Second)+1024)
	var (
		errs  atomic.Int64
		fives atomic.Int64
		total atomic.Int64
		wg    sync.WaitGroup
	)
	requests := make(chan struct{}, rps*4)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range requests {
				start := time.Now()
				status, err := post(ts.URL+"/v1/chat/completions", requestBody())
				elapsed := time.Since(start)
				total.Add(1)
				if err != nil {
					errs.Add(1)
					continue
				}
				if status >= 500 {
					fives.Add(1)
					continue
				}
				latencies <- elapsed
			}
		}()
	}

loop:
	for {
		select {
		case <-stop:
			break loop
		case <-tick.C:
			select {
			case requests <- struct{}{}:
			default:
				// queue full — measurement signal that the proxy is
				// dropping below RPS. Count as a transport error.
				errs.Add(1)
			}
		}
	}
	close(requests)
	wg.Wait()
	close(latencies)

	// Drain timings.
	samples := make([]time.Duration, 0, total.Load())
	for l := range latencies {
		samples = append(samples, l)
	}

	if errs.Load() > 0 {
		t.Errorf("transport errors: %d / %d", errs.Load(), total.Load())
	}
	if fives.Load() > 0 {
		t.Errorf("5xx responses: %d / %d", fives.Load(), total.Load())
	}
	if len(samples) == 0 {
		t.Fatal("no successful samples")
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)*50/100]
	p95 := samples[len(samples)*95/100]
	p99 := samples[len(samples)*99/100]
	added99 := p99 - baseline
	if added99 < 0 {
		added99 = 0
	}

	t.Logf("requests=%d errors=%d 5xx=%d baseline=%v p50=%v p95=%v p99=%v p99-baseline=%v",
		total.Load(), errs.Load(), fives.Load(), baseline, p50, p95, p99, added99)

	if added99 > p99Budget {
		t.Errorf("p99 added latency = %v, want <= %v", added99, p99Budget)
	}

	// Goroutine leak check. Allow some headroom — http.Transport
	// pools, batch span processors, etc. take a moment to wind down.
	// Wait briefly first so transient teardown goroutines drain.
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	if growth := after - goroutinesBefore; growth > goroutinesBefore { // > 200 %
		t.Errorf("goroutine growth: %d → %d (+%d). Likely leak.",
			goroutinesBefore, after, growth)
	}
}

// measureBaseline samples direct upstream latency (no proxy) so we
// can attribute added latency from the proxy alone. Takes 50 samples
// and returns the median to suppress single-shot dialer noise.
func measureBaseline(t *testing.T, url string) time.Duration {
	t.Helper()
	samples := make([]time.Duration, 0, 50)
	for i := 0; i < 50; i++ {
		start := time.Now()
		_, _ = post(url, requestBody())
		samples = append(samples, time.Since(start))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}

func requestBody() string {
	return `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
}

func post(url, body string) (status int, err error) {
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
