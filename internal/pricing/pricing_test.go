package pricing

import "testing"

func TestCostExactModel(t *testing.T) {
	t.Parallel()
	usd, ok := Default().Cost("openai", "gpt-4o-mini", 1_000_000, 500_000)
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
	usd, ok := Default().Cost("openai", "gpt-4o-mini-2024-07-18", 1_000_000, 0)
	if !ok {
		t.Fatal("expected ok")
	}
	if !approxEqual(usd, 0.150) {
		t.Errorf("expected gpt-4o-mini rate, got %v", usd)
	}
}

func TestCostUnknownReturnsFalse(t *testing.T) {
	t.Parallel()
	if _, ok := Default().Cost("openai", "made-up-model", 1, 1); ok {
		t.Fatal("expected ok=false")
	}
	if _, ok := Default().Cost("cohere", "command-r", 1, 1); ok {
		t.Fatal("unknown system should be ok=false")
	}
}

func TestCostCaseInsensitive(t *testing.T) {
	t.Parallel()
	usd, ok := Default().Cost("openai", "GPT-4o-mini", 1_000_000, 0)
	if !ok || !approxEqual(usd, 0.150) {
		t.Fatalf("case-insensitive lookup failed: usd=%v ok=%v", usd, ok)
	}
}

func TestCostDetailedPricesCachedInputSeparately(t *testing.T) {
	t.Parallel()
	tbl := newTable(map[string]map[string]Rate{
		"openai": {
			"gpt-test": {
				InputUSDPerMTok:         10,
				OutputUSDPerMTok:        20,
				CachedInputUSDPerMTok:   2,
				CacheCreationUSDPerMTok: 12,
			},
		},
	})
	got, ok := tbl.CostDetailed("openai", "gpt-test", 1_000_000, 500_000, 250_000, 100_000)
	if !ok {
		t.Fatal("model not priced")
	}
	// 650k normal ($6.50) + 250k read ($0.50) + 100k write ($1.20) + output ($10).
	if got != 18.2 {
		t.Fatalf("cost = %v, want 18.2", got)
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
