package pricing_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/pricing"
)

// TestTableDefaultCarriesEmbedded asserts the embedded prices.yaml is
// reachable without any operator configuration.
func TestTableDefaultCarriesEmbedded(t *testing.T) {
	t.Parallel()
	tbl := pricing.Default()
	usd, ok := tbl.Cost("openai", "gpt-4o-mini", 1_000_000, 0)
	if !ok {
		t.Fatal("default table missing built-in openai/gpt-4o-mini rate")
	}
	if !approxEqual(usd, 0.150) {
		t.Errorf("default input rate for gpt-4o-mini = %v, want 0.150", usd)
	}
}

// TestTableOverrideReplacesBuiltIn proves the externalized pricing
// knob is wired end-to-end: an operator-supplied file changes the
// cost the proxy will record for matching models. The advertised
// "production deployments should override per their negotiated rate"
// is no longer aspirational.
func TestTableOverrideReplacesBuiltIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.yaml")
	body := []byte(`
openai:
  gpt-4o-mini:
    input_usd_per_mtok: 0.05
    output_usd_per_mtok: 0.20
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	tbl, err := pricing.Load(path, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	usd, ok := tbl.Cost("openai", "gpt-4o-mini", 1_000_000, 0)
	if !ok {
		t.Fatal("override didn't expose openai/gpt-4o-mini")
	}
	if !approxEqual(usd, 0.05) {
		t.Errorf("overridden input rate = %v, want 0.05", usd)
	}
}

// TestTableOverrideFallsBackToBuiltIn asserts that an override file
// covering only some models leaves the built-in entries intact for
// the unspecified ones (additive merge, not whole-table replacement).
func TestTableOverrideFallsBackToBuiltIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.yaml")
	body := []byte(`
anthropic:
  claude-3-5-sonnet:
    input_usd_per_mtok: 1.50
    output_usd_per_mtok: 7.50
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	tbl, err := pricing.Load(path, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Override engaged for the anthropic family it covers.
	usd, ok := tbl.Cost("anthropic", "claude-3-5-sonnet", 1_000_000, 0)
	if !ok || !approxEqual(usd, 1.50) {
		t.Errorf("override on claude-3-5-sonnet not applied: usd=%v ok=%v", usd, ok)
	}
	// Fallback engaged for openai/gpt-4o-mini, which the override file
	// did not touch.
	usd, ok = tbl.Cost("openai", "gpt-4o-mini", 1_000_000, 0)
	if !ok || !approxEqual(usd, 0.150) {
		t.Errorf("built-in fallback for gpt-4o-mini lost: usd=%v ok=%v", usd, ok)
	}
}

// TestTableLoadMissingFileFailClosed: with fail_open=false and a
// missing path, Load returns an error and the caller is expected to
// refuse startup.
func TestTableLoadMissingFileFailClosed(t *testing.T) {
	t.Parallel()
	if _, err := pricing.Load("/nonexistent/prices.yaml", false); err == nil {
		t.Fatal("expected error on missing file with fail_open=false")
	}
}

// TestTableLoadMissingFileFailOpen: with fail_open=true and a missing
// path, Load returns the default table without erroring — the operator
// is opting into degraded cost reporting rather than process failure.
func TestTableLoadMissingFileFailOpen(t *testing.T) {
	t.Parallel()
	tbl, err := pricing.Load("/nonexistent/prices.yaml", true)
	if err != nil {
		t.Fatalf("fail_open=true should not error on missing file: %v", err)
	}
	usd, ok := tbl.Cost("openai", "gpt-4o-mini", 1_000_000, 0)
	if !ok || !approxEqual(usd, 0.150) {
		t.Errorf("fail_open fallback did not restore built-in: usd=%v ok=%v", usd, ok)
	}
}

// TestTableLoadMalformedFailClosed: with fail_open=false and a
// malformed file, Load returns an error.
func TestTableLoadMalformedFailClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.yaml")
	if err := os.WriteFile(path, []byte("not: valid: yaml: [::]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pricing.Load(path, false); err == nil {
		t.Fatal("expected error on malformed YAML with fail_open=false")
	}
}

// TestTableLoadEmptyPath returns the default table.
func TestTableLoadEmptyPath(t *testing.T) {
	t.Parallel()
	tbl, err := pricing.Load("", false)
	if err != nil {
		t.Fatalf("empty path should not error: %v", err)
	}
	if usd, ok := tbl.Cost("openai", "gpt-4o-mini", 1_000_000, 0); !ok || !approxEqual(usd, 0.150) {
		t.Errorf("empty path didn't yield default table: usd=%v ok=%v", usd, ok)
	}
}

// TestPricingEqualLengthIsDeterministic pins down the lookup contract
// after the A17 fix: prefix matching is deterministic across calls and
// across goroutines, with no dependence on Go's randomized map
// iteration order.
//
// Two flavours are exercised:
//
//  1. Same-length distinct prefixes that each match a different probe.
//     With strict-prefix-of, two distinct equal-length strings cannot
//     both prefix the same model, so the tie window is narrow — but
//     the data path must still walk these in a stable order so the
//     "longest wins" tiebreak doesn't drift across process restarts.
//
//  2. Mixed-length prefixes where multiple candidates match the same
//     probe. The longest must win every single call (1000 serial +
//     64 goroutines x 200 calls).
//
// Pre-fix, lookup ranged the map directly. While the current `>`
// comparison happens to suppress the visible tie, the structural
// nondeterminism — map-order in the catalogue scan — is the latent
// correctness bomb the fix removes. The post-fix slice walk is
// deterministic by construction.
//
// Note: lookup() lowercases the model string before matching, so
// prefixes in the override must already be lowercase or they're
// unreachable.
func TestPricingEqualLengthIsDeterministic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.yaml")
	// Prefix shape:
	//   - "moda" / "modb": equal-length (len 4), distinct, each
	//     matches only its own probe. Sanity baseline.
	//   - "x-aa" / "x-bb" (len 4) + "x-aaab" (len 6): probe
	//     "x-aaab-1" matches both "x-aa" and "x-aaab"; "x-aaab"
	//     (longer) must win every call. Probe "x-aac-1" matches
	//     only "x-aa".
	//   - "ab" + "ab-extra" (len 2 + 8): probe "ab-extra-tail"
	//     matches both, the longer must win.
	body := []byte(`
synthetic:
  moda:
    input_usd_per_mtok: 1.000
    output_usd_per_mtok: 0
  modb:
    input_usd_per_mtok: 2.000
    output_usd_per_mtok: 0
  x-aa:
    input_usd_per_mtok: 10.000
    output_usd_per_mtok: 0
  x-bb:
    input_usd_per_mtok: 20.000
    output_usd_per_mtok: 0
  x-aaab:
    input_usd_per_mtok: 99.000
    output_usd_per_mtok: 0
  ab:
    input_usd_per_mtok: 3.000
    output_usd_per_mtok: 0
  ab-extra:
    input_usd_per_mtok: 7.000
    output_usd_per_mtok: 0
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	tbl, err := pricing.Load(path, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cases := []struct {
		model string
		want  float64
	}{
		// Single-match probes against equal-length prefixes.
		{"moda", 1.000},
		{"modb", 2.000},
		// Longest-prefix-wins over an equal-length-tie field. Both
		// "x-aa" and "x-bb" are length 4; "x-aaab" is length 6.
		// Probe must resolve to "x-aaab".
		{"x-aaab-1", 99.000},
		// Probe that only matches "x-aa" (length 4) and could
		// confuse a buggy walker if it conflated "x-aaab" with
		// "x-aa" tie ordering.
		{"x-aac-1", 10.000},
		// Mixed-length walk: probe matches "ab" (len 2) AND
		// "ab-extra" (len 8). Longer must win on every call,
		// regardless of which order the prefixes happen to sit in
		// the underlying data structure.
		{"ab-extra-tail", 7.000},
		// Probe matches "ab" only.
		{"abz", 3.000},
	}

	// Serial: 1000 iterations per case must all produce the same value.
	const iters = 1000
	for _, tc := range cases {
		first, ok := tbl.Cost("synthetic", tc.model, 1_000_000, 0)
		if !ok {
			t.Fatalf("%q: no match", tc.model)
		}
		if !approxEqual(first, tc.want) {
			t.Fatalf("%q baseline = %v, want %v", tc.model, first, tc.want)
		}
		for i := 0; i < iters; i++ {
			got, ok := tbl.Cost("synthetic", tc.model, 1_000_000, 0)
			if !ok {
				t.Fatalf("%q iter %d: ok=false", tc.model, i)
			}
			if got != first {
				t.Fatalf("%q iter %d: cost flipped %v -> %v (nondeterministic)",
					tc.model, i, first, got)
			}
		}
	}

	// Concurrent: 64 goroutines x 200 calls per case must all agree.
	const fanout, perGo = 64, 200
	for _, tc := range cases {
		tc := tc
		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			seen   = map[float64]struct{}{}
			notOK  int
			expect float64
		)
		expect = tc.want
		for g := 0; g < fanout; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perGo; i++ {
					got, ok := tbl.Cost("synthetic", tc.model, 1_000_000, 0)
					mu.Lock()
					if !ok {
						notOK++
					} else {
						seen[got] = struct{}{}
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if notOK > 0 {
			t.Fatalf("%q: %d concurrent calls returned ok=false", tc.model, notOK)
		}
		if len(seen) != 1 {
			t.Fatalf("%q: concurrent calls observed %d distinct costs: %v",
				tc.model, len(seen), seen)
		}
		for v := range seen {
			if !approxEqual(v, expect) {
				t.Fatalf("%q: concurrent cost = %v, want %v", tc.model, v, expect)
			}
		}
	}
}

func approxEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
