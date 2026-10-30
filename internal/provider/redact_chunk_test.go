package provider

import (
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/redact"
)

// TestRedactChunkBoundary verifies redaction still masks secrets split
// across assembled stream chunks (D32).
func TestRedactChunkBoundary(t *testing.T) {
	t.Parallel()
	secret := "sk-abcdefghijklmnopqrstuvwxyz1234567890"
	fn := redact.Func(redact.ProfileDefault)
	var b strings.Builder
	// Split sk- token across three deltas like SSE chunks would.
	parts := []string{secret[:5], secret[5:15], secret[15:]}
	for _, p := range parts {
		b.WriteString(p)
	}
	final := fn(b.String())
	if strings.Contains(final, secret) {
		t.Fatalf("assembled secret not redacted: %q", final)
	}
	if !strings.Contains(final, redact.Mask) {
		t.Fatalf("expected %q in %q", redact.Mask, final)
	}
}
