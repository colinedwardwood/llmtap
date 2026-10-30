package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadRejectsUnknownFields pins D15: a typo'd key must fail the
// load, not silently leave the default in place. Before strict
// decoding, `max_inflight: 4` (missing underscore) validated fine and
// shipped an uncapped proxy.
func TestLoadRejectsUnknownFields(t *testing.T) {
	path := writeTempConfig(t, `
upstreams:
  - name: openai
    prefix: /v1
    target: https://api.openai.com
    provider: openai
    max_inflight: 4
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("config with unknown field max_inflight loaded without error")
	}
	if !strings.Contains(err.Error(), "max_inflight") {
		t.Fatalf("error should name the unknown field, got: %v", err)
	}
}

// TestValidateTargetURLShape pins the target sanity checks: url.Parse
// accepts nearly anything, so a target without a scheme ("api.openai.com"
// parses as a relative path) must be rejected at validate time instead
// of dialing "" at runtime.
func TestValidateTargetURLShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		target string
		wantOK bool
	}{
		{"https://api.openai.com", true},
		{"http://localhost:8080", true},
		{"api.openai.com", false},
		{"ftp://api.openai.com", false},
		{"https://", false},
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.Upstreams = []Upstream{{
			Name: "u", Prefix: "/v1", Target: tc.target, Provider: ProviderOpenAI,
		}}
		err := cfg.Validate()
		if tc.wantOK && err != nil {
			t.Errorf("target %q: unexpected error: %v", tc.target, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("target %q: validated but should have been rejected", tc.target)
		}
	}
}

// TestExampleConfigLoadsStrict keeps config.example.yaml honest: every
// key in the shipped example must exist in the schema, or strict
// decoding would reject the operator's copy-paste starting point.
func TestExampleConfigLoadsStrict(t *testing.T) {
	t.Parallel()

	_, thisFile, _, _ := runtime.Caller(0)
	example := filepath.Join(filepath.Dir(thisFile), "..", "..", "config.example.yaml")
	if _, err := Load(example); err != nil {
		t.Fatalf("config.example.yaml failed strict load: %v", err)
	}
}
