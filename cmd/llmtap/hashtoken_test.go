package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/auth"
)

// TestHashTokenCommandRoundtrip pipes a plaintext token into runHashToken
// and round-trips the printed PHC hash through auth.Verify. The CLI is
// the only ergonomic way to produce a hash for the config file, so it
// must actually produce something the runtime accepts.
func TestHashTokenCommandRoundtrip(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := runHashToken(nil, strings.NewReader("my-real-token\n"), &stdout, &stderr); err != nil {
		t.Fatalf("runHashToken: %v", err)
	}
	encoded := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("output not a PHC hash: %q", encoded)
	}
	ok, err := auth.Verify("my-real-token", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("freshly emitted hash failed to verify its own plaintext")
	}
}

func TestHashTokenCommandRefusesEmptyStdin(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := runHashToken(nil, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Error("expected error on empty stdin")
	}
}
