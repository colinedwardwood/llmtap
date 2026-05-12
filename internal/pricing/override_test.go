package pricing_test

import (
	"os"
	"path/filepath"
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

func approxEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
