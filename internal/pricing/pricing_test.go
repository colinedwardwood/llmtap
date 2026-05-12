package pricing

import "testing"

func TestCostExactModel(t *testing.T) {
	t.Parallel()
	usd, ok := Cost("openai", "gpt-4o-mini", 1_000_000, 500_000)
	if !ok {
		t.Fatal("expected ok")
	}
	want := 0.150 + 0.5*0.600
	if !approxEqual(usd, want) {
		t.Errorf("usd = %v want %v", usd, want)
	}
}

func TestCostLongestPrefixWins(t *testing.T) {
	t.Parallel()
	// pinned snapshot id should match the gpt-4o-mini prefix, not gpt-4o
	usd, ok := Cost("openai", "gpt-4o-mini-2024-07-18", 1_000_000, 0)
	if !ok {
		t.Fatal("expected ok")
	}
	if !approxEqual(usd, 0.150) {
		t.Errorf("expected gpt-4o-mini rate, got %v", usd)
	}
}

func TestCostUnknownReturnsFalse(t *testing.T) {
	t.Parallel()
	if _, ok := Cost("openai", "made-up-model", 1, 1); ok {
		t.Fatal("expected ok=false")
	}
	if _, ok := Cost("cohere", "command-r", 1, 1); ok {
		t.Fatal("unknown system should be ok=false")
	}
}

func TestCostCaseInsensitive(t *testing.T) {
	t.Parallel()
	usd, ok := Cost("openai", "GPT-4o-mini", 1_000_000, 0)
	if !ok || !approxEqual(usd, 0.150) {
		t.Fatalf("case-insensitive lookup failed: usd=%v ok=%v", usd, ok)
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
