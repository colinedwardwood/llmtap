package config

import (
	"testing"
)

// TestMatchSegmentAware pins D6: prefix matching happens at path
// segment boundaries. Byte-prefix matching routed /v1foo into the /v1
// upstream, diverging from the enrichment layer's segment-strict view.
func TestMatchSegmentAware(t *testing.T) {
	t.Parallel()

	cfg := Default()

	if _, ok := cfg.Match("/v1foo/chat/completions"); ok {
		t.Fatal("/v1foo matched the /v1 prefix — matching is not segment-aware")
	}
	if got, ok := cfg.Match("/v1"); !ok || got.Name != "openai" {
		t.Fatalf("exact prefix match failed: %+v ok=%v", got, ok)
	}
	if got, ok := cfg.Match("/v1/chat/completions"); !ok || got.Name != "openai" {
		t.Fatalf("segment match failed: %+v ok=%v", got, ok)
	}
	if got, ok := cfg.Match("/anthropic/v1/messages"); !ok || got.Name != "anthropic" {
		t.Fatalf("anthropic match failed: %+v ok=%v", got, ok)
	}
}

// TestDefaultAnthropicStripsPrefix pins the fixed default: /anthropic
// is a routing-only mount and must be configured to strip.
func TestDefaultAnthropicStripsPrefix(t *testing.T) {
	t.Parallel()

	for _, u := range Default().Upstreams {
		switch u.Name {
		case "anthropic":
			if !u.StripPrefix {
				t.Error("default anthropic upstream must set strip_prefix: true (the documented SDK flow 404s otherwise)")
			}
		case "openai":
			if u.StripPrefix {
				t.Error("default openai upstream must NOT strip: /v1 is the upstream's real base path")
			}
		}
	}
}

// TestLogsCaptureModeAccepted pins D31: content.mode "logs" is wired to
// the OTel log pipeline and must validate like events/off.
func TestLogsCaptureModeAccepted(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Content.Mode = CaptureLogs
	if err := cfg.Validate(); err != nil {
		t.Fatalf("content.mode logs should validate: %v", err)
	}
}
