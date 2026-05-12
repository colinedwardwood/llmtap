// Package labels bounds the cardinality of high-risk metric labels.
//
// LLM provider model strings flow from client request bodies into metric
// labels. Without a bound, a hostile or buggy client can mint unlimited
// Prometheus/Mimir series and exhaust the operator's observability
// backend. NewModelLabel returns a normalizer that:
//
//  1. Lowercases and strips date/snapshot suffixes (so "gpt-4o-2024-08-06"
//     and "gpt-4o-2024-11-20" collapse to "gpt-4o").
//  2. Tracks the set of distinct normalized values observed and routes
//     any value past a configurable cap to the synthetic label "_other".
//
// Span attributes are not bound here — span cardinality is governed by
// retention. Only metric labels need this protection.
package labels

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// OtherLabel is the bucket assigned to model strings observed after the
// cardinality cap has been reached.
const OtherLabel = "_other"

// DefaultMaxCardinality is the recommended cap for production deployments.
// Two hundred distinct model families is generous for any single tenant;
// real users see ~10–30 at any given time.
const DefaultMaxCardinality = 200

// dateSuffix matches a trailing date-style or version snapshot suffix.
// Examples it strips:
//
//	-2024-08-06  (OpenAI style)
//	-20241022    (Anthropic style)
//	-v1, -v2     (occasionally seen on community providers)
var dateSuffix = regexp.MustCompile(`-(?:\d{4}-\d{2}-\d{2}|\d{8}|v\d+)$`)

// ModelLabel normalizes and caps the cardinality of model strings used as
// metric labels. The zero value is not usable; construct via NewModelLabel.
type ModelLabel struct {
	cap      int
	observed sync.Map // key: normalized string, value: struct{}
	size     atomic.Int32
}

// NewModelLabel returns a normalizer with the given hard cap on distinct
// accepted labels. If maxCardinality <= 0, DefaultMaxCardinality is used.
func NewModelLabel(maxCardinality int) *ModelLabel {
	if maxCardinality <= 0 {
		maxCardinality = DefaultMaxCardinality
	}
	return &ModelLabel{cap: maxCardinality}
}

// Normalize returns the bounded label for model. An empty input returns
// an empty string (the caller should omit the label entirely in that
// case). Inputs beyond the cap are routed to OtherLabel.
//
// Safe for concurrent use.
func (m *ModelLabel) Normalize(model string) string {
	if model == "" {
		return ""
	}
	norm := dateSuffix.ReplaceAllString(strings.ToLower(model), "")

	if _, seen := m.observed.Load(norm); seen {
		return norm
	}
	if int(m.size.Load()) >= m.cap {
		return OtherLabel
	}
	// LoadOrStore is the race-free admission. Only increment size when
	// we actually stored (winner of the race for this key).
	if _, loaded := m.observed.LoadOrStore(norm, struct{}{}); !loaded {
		// Re-check the cap after admission: under high concurrency,
		// multiple goroutines may have raced past the pre-check. The
		// post-check trims the overshoot back to the cap.
		if int(m.size.Add(1)) > m.cap {
			m.observed.Delete(norm)
			m.size.Add(-1)
			return OtherLabel
		}
	}
	return norm
}

// AcceptedCount returns the current number of distinct labels admitted.
// Test-only helper; cheap enough that exposing it isn't a footgun.
func (m *ModelLabel) AcceptedCount() int {
	return int(m.size.Load())
}
