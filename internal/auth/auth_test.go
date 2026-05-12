package auth_test

import (
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/auth"
)

func TestHashVerifyRoundtrip(t *testing.T) {
	t.Parallel()
	encoded, err := auth.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Errorf("encoded hash missing argon2id prefix: %q", encoded)
	}
	ok, err := auth.Verify("correct-horse-battery-staple", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("verify returned false for the correct password")
	}
}

func TestHashIsSalted(t *testing.T) {
	t.Parallel()
	a, err := auth.Hash("same-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	b, err := auth.Hash("same-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("hashing the same plaintext twice produced identical output — salt missing")
	}
}

func TestVerifyRejectsWrongPlaintext(t *testing.T) {
	t.Parallel()
	encoded, err := auth.Hash("real-token")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := auth.Verify("guessed-token", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("verify returned true for the wrong password")
	}
}

func TestVerifyMalformedEncoded(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2id$v=19$m=foo,t=bar,p=baz$salt$hash",
		"$bcrypt$10$abc$def",
	}
	for _, enc := range cases {
		if _, err := auth.Verify("anything", enc); err == nil {
			t.Errorf("expected error on malformed encoded %q", enc)
		}
	}
}

func TestVerifierAcceptsKnownToken(t *testing.T) {
	t.Parallel()
	h, err := auth.Hash("token-A")
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier([]string{h})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Verify("token-A") {
		t.Error("verifier rejected the token it was configured with")
	}
	if v.Verify("token-B") {
		t.Error("verifier accepted a token it wasn't configured with")
	}
}

func TestVerifierAcceptsAnyOfMultipleTokens(t *testing.T) {
	t.Parallel()
	hA, _ := auth.Hash("alpha")
	hB, _ := auth.Hash("bravo")
	v, err := auth.NewVerifier([]string{hA, hB})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Verify("alpha") {
		t.Error("multi-hash verifier rejected first token")
	}
	if !v.Verify("bravo") {
		t.Error("multi-hash verifier rejected second token")
	}
	if v.Verify("charlie") {
		t.Error("multi-hash verifier accepted an unknown token")
	}
}

func TestVerifierRejectsEmptyToken(t *testing.T) {
	t.Parallel()
	h, _ := auth.Hash("real")
	v, _ := auth.NewVerifier([]string{h})
	if v.Verify("") {
		t.Error("empty token must not match")
	}
}

func TestVerifierEnabled(t *testing.T) {
	t.Parallel()
	empty, err := auth.NewVerifier(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Enabled() {
		t.Error("Enabled() must return false when no tokens are configured")
	}
	h, _ := auth.Hash("real")
	on, _ := auth.NewVerifier([]string{h})
	if !on.Enabled() {
		t.Error("Enabled() must return true when at least one token is configured")
	}
}

func TestNewVerifierRejectsMalformedTokenAtConstruction(t *testing.T) {
	t.Parallel()
	if _, err := auth.NewVerifier([]string{"not-a-hash"}); err == nil {
		t.Error("NewVerifier must reject malformed encoded tokens at construction time")
	}
}
