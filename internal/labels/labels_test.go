package labels_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/labels"
)

func TestModelNormalizeStripsDateSnapshots(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"gpt-4o-2024-08-06", "gpt-4o"},
		{"gpt-4o-2024-11-20", "gpt-4o"},
		{"gpt-4o-mini-2024-07-18", "gpt-4o-mini"},
		{"claude-3-5-sonnet-20241022", "claude-3-5-sonnet"},
		{"claude-3-5-haiku-20241022", "claude-3-5-haiku"},
		{"gpt-4-turbo-2024-04-09", "gpt-4-turbo"},
	}
	m := labels.NewModelLabel(100)
	for _, tc := range cases {
		if got := m.Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestModelNormalizeLowercases(t *testing.T) {
	t.Parallel()
	m := labels.NewModelLabel(100)
	if got := m.Normalize("GPT-4O"); got != "gpt-4o" {
		t.Errorf("Normalize(GPT-4O) = %q, want gpt-4o", got)
	}
}

func TestModelNormalizeEmptyPassthrough(t *testing.T) {
	t.Parallel()
	m := labels.NewModelLabel(100)
	if got := m.Normalize(""); got != "" {
		t.Errorf("Normalize(\"\") = %q, want \"\"", got)
	}
}

func TestModelCardinalityCap(t *testing.T) {
	t.Parallel()
	const cap = 50
	m := labels.NewModelLabel(cap)

	// First `cap` distinct models accepted as-is.
	seen := map[string]bool{}
	for i := 0; i < cap; i++ {
		out := m.Normalize(fmt.Sprintf("family-%d", i))
		if out == "_other" {
			t.Fatalf("rejected before cap reached at i=%d", i)
		}
		seen[out] = true
	}
	if len(seen) != cap {
		t.Fatalf("expected %d distinct accepted labels, got %d", cap, len(seen))
	}

	// Beyond cap: new models routed to "_other".
	for i := cap; i < cap+200; i++ {
		out := m.Normalize(fmt.Sprintf("family-%d", i))
		if out != "_other" {
			t.Errorf("over-cap model %d returned %q, want _other", i, out)
		}
	}

	// Previously-seen models still return their accepted form.
	if out := m.Normalize("family-0"); out != "family-0" {
		t.Errorf("previously-accepted label flipped: got %q", out)
	}
}

func TestModelCardinalityCapIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	const cap = 100
	m := labels.NewModelLabel(cap)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = m.Normalize(fmt.Sprintf("g%d-m%d", g, j))
			}
		}(i)
	}
	wg.Wait()
	// No assertion on exact accepted set — race-y by design — but the
	// run must complete without data races (go test -race catches that)
	// and the size must not exceed cap.
	if got := m.AcceptedCount(); got > cap {
		t.Errorf("accepted %d distinct labels exceeds cap %d", got, cap)
	}
}
