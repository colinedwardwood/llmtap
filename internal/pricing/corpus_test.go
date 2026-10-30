package pricing

import "testing"

// TestCorpusModelsAllPriced is the freshness gate for the embedded
// catalogue (review finding R9): the table shipped as a stale snapshot
// and unknown models missed silently, so the cost dashboard undercounted
// exactly as new models rolled out. Every model ID below is shaped like
// what the OpenAI / Anthropic APIs return in responses today (including
// dated snapshot pins, which resolve via longest-prefix-match). When a
// provider ships a new family, add it here — the test failing is the
// reminder to refresh prices.yaml and bump pricing.Snapshot.
func TestCorpusModelsAllPriced(t *testing.T) {
	t.Parallel()

	corpus := map[string][]string{
		"openai": {
			// current flagship / general
			"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
			"gpt-5.5", "gpt-5.5-mini", "gpt-5.5-nano", "gpt-5.5-pro",
			"gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.4-pro",
			"gpt-5.3-codex", "gpt-5.2", "gpt-5.1",
			"gpt-5", "gpt-5-mini", "gpt-5-nano",
			"gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano",
			// snapshot pins inherit the family rate by prefix
			"gpt-5-2025-08-07", "gpt-4.1-2025-04-14",
			// reasoning
			"o3", "o3-pro", "o3-mini", "o4-mini",
			"o1", "o1-pro", "o1-mini", "o1-preview",
			// legacy chat + embeddings
			"gpt-4o", "gpt-4o-mini", "gpt-4o-mini-2024-07-18",
			"gpt-4-turbo", "gpt-4", "gpt-3.5-turbo",
			"text-embedding-3-small", "text-embedding-3-large",
			"text-embedding-ada-002",
		},
		"anthropic": {
			// current generation
			"claude-fable-5", "claude-mythos-5",
			"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
			"claude-opus-4-5", "claude-opus-4-1",
			"claude-sonnet-5", "claude-sonnet-4-6", "claude-sonnet-4-5",
			"claude-haiku-4-5",
			// snapshot pins
			"claude-opus-4-8-20260528", "claude-sonnet-4-5-20250929",
			"claude-haiku-4-5-20251001",
			// legacy naming
			"claude-3-7-sonnet-20250219",
			"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022",
			"claude-3-opus-20240229", "claude-3-haiku-20240307",
		},
	}

	table := Default()
	for system, models := range corpus {
		for _, model := range models {
			if _, ok := table.Cost(system, model, 1000, 1000); !ok {
				t.Errorf("no price for %s/%s — refresh prices.yaml and bump pricing.Snapshot", system, model)
			}
		}
	}
}

// TestOpusFourPrefixDoesNotSwallowMinorVersions guards the trickiest
// longest-prefix hazard in the catalogue: opus 4.1 costs 3x opus 4.5+,
// and both share the "claude-opus-4" prefix. The dated pins must land
// on their own minor version's rate.
func TestOpusFourPrefixDoesNotSwallowMinorVersions(t *testing.T) {
	t.Parallel()

	table := Default()
	// 1M input + 1M output at 5/25 = $30; at 15/75 = $90.
	if usd, ok := table.Cost("anthropic", "claude-opus-4-8-20260528", 1_000_000, 1_000_000); !ok || usd != 30 {
		t.Errorf("opus-4-8 pin priced at $%v (ok=%v), want $30", usd, ok)
	}
	if usd, ok := table.Cost("anthropic", "claude-opus-4-1-20250805", 1_000_000, 1_000_000); !ok || usd != 90 {
		t.Errorf("opus-4-1 pin priced at $%v (ok=%v), want $90", usd, ok)
	}
}
