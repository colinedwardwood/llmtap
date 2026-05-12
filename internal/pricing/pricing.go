// Package pricing converts (system, model, tokens) into USD cost.
//
// Prices are denominated per million tokens, matching how upstream providers
// publish them. Unknown models return (0, false); callers should not record a
// cost in that case so dashboards stay honest.
package pricing

import "strings"

// Rate is a model's input/output price in USD per 1M tokens.
type Rate struct {
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
}

// Cost returns the USD cost for the given token usage. ok is false if no
// price exists for (system, model); callers should skip cost emission.
func Cost(system, model string, inputTokens, outputTokens int64) (usd float64, ok bool) {
	r, found := lookup(system, model)
	if !found {
		return 0, false
	}
	usd = (float64(inputTokens)/1_000_000)*r.InputUSDPerMTok +
		(float64(outputTokens)/1_000_000)*r.OutputUSDPerMTok
	return usd, true
}

// lookup matches longest model-prefix first so that snapshot-pinned model IDs
// (e.g. "gpt-4o-mini-2024-07-18") inherit their family's price.
func lookup(system, model string) (Rate, bool) {
	table, ok := tables[system]
	if !ok {
		return Rate{}, false
	}
	model = strings.ToLower(model)
	var (
		bestRate Rate
		bestLen  = -1
	)
	for prefix, r := range table {
		if !strings.HasPrefix(model, prefix) {
			continue
		}
		if len(prefix) > bestLen {
			bestRate = r
			bestLen = len(prefix)
		}
	}
	return bestRate, bestLen >= 0
}

// tables holds the built-in price catalogue. Values are the public list price
// at time of release; production deployments should override per their
// negotiated rate. Source: provider pricing pages, snapshot 2025-Q4.
var tables = map[string]map[string]Rate{
	"openai": {
		"gpt-4o-mini":   {InputUSDPerMTok: 0.150, OutputUSDPerMTok: 0.600},
		"gpt-4o":        {InputUSDPerMTok: 2.500, OutputUSDPerMTok: 10.000},
		"gpt-4-turbo":   {InputUSDPerMTok: 10.000, OutputUSDPerMTok: 30.000},
		"gpt-4":         {InputUSDPerMTok: 30.000, OutputUSDPerMTok: 60.000},
		"gpt-3.5-turbo": {InputUSDPerMTok: 0.500, OutputUSDPerMTok: 1.500},
		"o1-mini":       {InputUSDPerMTok: 3.000, OutputUSDPerMTok: 12.000},
		"o1-preview":    {InputUSDPerMTok: 15.000, OutputUSDPerMTok: 60.000},
		"o1":            {InputUSDPerMTok: 15.000, OutputUSDPerMTok: 60.000},
		"o3-mini":       {InputUSDPerMTok: 1.100, OutputUSDPerMTok: 4.400},
		// Embedding models price per input only; output is conventionally 0.
		"text-embedding-3-small": {InputUSDPerMTok: 0.020},
		"text-embedding-3-large": {InputUSDPerMTok: 0.130},
		"text-embedding-ada-002": {InputUSDPerMTok: 0.100},
	},
	"anthropic": {
		"claude-3-5-haiku":  {InputUSDPerMTok: 0.800, OutputUSDPerMTok: 4.000},
		"claude-3-5-sonnet": {InputUSDPerMTok: 3.000, OutputUSDPerMTok: 15.000},
		"claude-3-opus":     {InputUSDPerMTok: 15.000, OutputUSDPerMTok: 75.000},
		"claude-3-sonnet":   {InputUSDPerMTok: 3.000, OutputUSDPerMTok: 15.000},
		"claude-3-haiku":    {InputUSDPerMTok: 0.250, OutputUSDPerMTok: 1.250},
	},
}
