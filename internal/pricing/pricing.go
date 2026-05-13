// Package pricing converts (system, model, tokens) into USD cost.
//
// Prices are denominated per million tokens, matching how upstream
// providers publish them. The built-in catalogue is the embedded
// `prices.yaml`; operators override per their negotiated rate by
// pointing `pricing.path` at a YAML file with the same shape — the
// file is merged on top of the built-ins, leaving unspecified model
// families at their default rates. Unknown models return (0, false);
// callers should not record a cost in that case so dashboards stay
// honest.
package pricing

import (
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed prices.yaml
var embeddedPrices []byte

// Rate is a model's input/output price in USD per 1M tokens. YAML
// field names match the documented operator-facing schema.
type Rate struct {
	InputUSDPerMTok  float64 `yaml:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `yaml:"output_usd_per_mtok"`
}

// Table is the active pricing catalogue. Construct via Default for the
// embedded defaults, or via Load to merge an operator override on top.
type Table struct {
	// rates is keyed by gen_ai.system then by model-prefix. The map
	// preserves O(1) system membership and supports the merge path
	// in Load.
	rates map[string]map[string]Rate
	// sorted is the deterministic walk order for lookup, built once
	// at construction time. Per-system, prefixes are ordered by
	// length (descending) and then lexicographically (ascending);
	// the first match wins. This removes the latent dependence on
	// Go's randomized map iteration order in the cost-computation
	// data path.
	sorted map[string][]prefixedRate
}

// prefixedRate is a (prefix, rate) pair held in the per-system sorted
// walk order. Kept private; the only consumer is Table.lookup.
type prefixedRate struct {
	prefix string
	rate   Rate
}

// defaultTable is the embedded built-in catalogue. It is constructed
// once at package init and never mutated, so concurrent readers of
// Default() are safe.
var defaultTable *Table

func init() {
	var parsed map[string]map[string]Rate
	if err := yaml.Unmarshal(embeddedPrices, &parsed); err != nil {
		panic(fmt.Errorf("pricing: embedded prices.yaml malformed: %w", err))
	}
	defaultTable = newTable(parsed)
}

// newTable builds a Table from a parsed (system, prefix, rate) map and
// precomputes the deterministic per-system walk order. Construction
// allocates; lookups are read-only and lock-free.
func newTable(rates map[string]map[string]Rate) *Table {
	sorted := make(map[string][]prefixedRate, len(rates))
	for sys, models := range rates {
		entries := make([]prefixedRate, 0, len(models))
		for prefix, rate := range models {
			entries = append(entries, prefixedRate{prefix: prefix, rate: rate})
		}
		sort.Slice(entries, func(i, j int) bool {
			if len(entries[i].prefix) != len(entries[j].prefix) {
				return len(entries[i].prefix) > len(entries[j].prefix)
			}
			return entries[i].prefix < entries[j].prefix
		})
		sorted[sys] = entries
	}
	return &Table{rates: rates, sorted: sorted}
}

// Default returns the embedded built-in pricing catalogue. The returned
// table is shared — do not mutate it.
func Default() *Table {
	return defaultTable
}

// Load builds a Table that layers an operator-supplied YAML override
// on top of the built-in catalogue. Empty path returns Default().
//
// failOpen controls behaviour when the file is missing or malformed:
//   - false (recommended for production): return an error so the
//     operator notices.
//   - true: silently fall back to Default() so a typo'd path doesn't
//     crash the proxy.
func Load(path string, failOpen bool) (*Table, error) {
	if path == "" {
		return Default(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if failOpen {
			return Default(), nil
		}
		return nil, fmt.Errorf("read pricing file %q: %w", path, err)
	}
	var override map[string]map[string]Rate
	if err := yaml.Unmarshal(raw, &override); err != nil {
		if failOpen {
			return Default(), nil
		}
		return nil, fmt.Errorf("parse pricing file %q: %w", path, err)
	}

	// Merge: deep-copy the defaults, then layer the override per
	// (system, model-prefix) key. The override wins per matching key
	// without dropping any unspecified default.
	merged := make(map[string]map[string]Rate, len(defaultTable.rates))
	for sys, models := range defaultTable.rates {
		clone := make(map[string]Rate, len(models))
		for k, v := range models {
			clone[k] = v
		}
		merged[sys] = clone
	}
	for sys, models := range override {
		if merged[sys] == nil {
			merged[sys] = make(map[string]Rate, len(models))
		}
		for k, v := range models {
			merged[sys][k] = v
		}
	}
	return newTable(merged), nil
}

// Cost returns the USD cost for the given token usage against this
// table. ok is false if no rate exists for (system, model); callers
// should skip cost emission in that case.
func (t *Table) Cost(system, model string, inputTokens, outputTokens int64) (usd float64, ok bool) {
	r, found := t.lookup(system, model)
	if !found {
		return 0, false
	}
	usd = (float64(inputTokens)/1_000_000)*r.InputUSDPerMTok +
		(float64(outputTokens)/1_000_000)*r.OutputUSDPerMTok
	return usd, true
}

func (t *Table) lookup(system, model string) (Rate, bool) {
	entries, ok := t.sorted[system]
	if !ok {
		return Rate{}, false
	}
	model = strings.ToLower(model)
	// entries is pre-sorted (length desc, then prefix asc); the first
	// HasPrefix hit is the longest-prefix winner with a stable
	// lexicographic tiebreak — no map-iteration order in the data
	// path.
	for _, e := range entries {
		if strings.HasPrefix(model, e.prefix) {
			return e.rate, true
		}
	}
	return Rate{}, false
}

// Cost is a backwards-compatible package-level alias that resolves
// against the embedded built-in table. Code paths that need an
// operator-overridable table should hold a *Table directly.
func Cost(system, model string, inputTokens, outputTokens int64) (usd float64, ok bool) {
	return defaultTable.Cost(system, model, inputTokens, outputTokens)
}
