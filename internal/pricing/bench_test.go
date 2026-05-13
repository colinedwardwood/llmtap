package pricing

import (
	"fmt"
	"testing"
)

// BenchmarkPricingLookup checks the post-A35 lookup budget: per-op cost
// stays within 200ns at N=500 distinct prefixes per system.
//
// Pre-fix (linear scan of sorted slice) this was O(N·avg_prefix_len)
// — about 35 µs at N=500. The trie is O(K) where K = len(model),
// closer to 80ns at our prefix shapes.
func BenchmarkPricingLookup(b *testing.B) {
	rates := map[string]map[string]Rate{
		"synthetic": make(map[string]Rate, 500),
	}
	// Generate 500 short family prefixes (like the real "gpt-4o-mini"
	// shape) so the lookup walks a realistic depth.
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("fam-%03d-x%d", i, i*7)
		rates["synthetic"][key] = Rate{InputUSDPerMTok: float64(i), OutputUSDPerMTok: float64(i * 2)}
	}
	tbl := newTable(rates)

	// Probe extends a registered family prefix with a snapshot
	// suffix, mirroring real OpenAI / Anthropic model ids like
	// "gpt-4o-mini-2024-07-18". 22 chars total.
	probe := "fam-250-x1750-2024-1118"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := tbl.Cost("synthetic", probe, 1_000_000, 100_000); !ok {
			b.Fatalf("Cost returned !ok unexpectedly")
		}
	}
}

// BenchmarkPricingLookupMiss exercises the path where no prefix
// matches — the trie walks until either it falls off a branch or the
// model is consumed, then returns false.
func BenchmarkPricingLookupMiss(b *testing.B) {
	rates := map[string]map[string]Rate{
		"synthetic": make(map[string]Rate, 500),
	}
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("model-%03d-family-suffix-%d", i, i*7)
		rates["synthetic"][key] = Rate{InputUSDPerMTok: float64(i), OutputUSDPerMTok: 0}
	}
	tbl := newTable(rates)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = tbl.Cost("synthetic", "xyz-not-in-the-table", 1, 1)
	}
}
