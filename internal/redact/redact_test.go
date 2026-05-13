package redact_test

import (
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/redact"
)

func TestApplyOffIsPassthrough(t *testing.T) {
	t.Parallel()
	in := "my key is sk-AAAAAAAAAAAAAAAAAAAA and my email is alice@example.com"
	if got := redact.Apply(in, redact.ProfileOff); got != in {
		t.Errorf("ProfileOff mutated input: %q", got)
	}
	if got := redact.Apply(in, ""); got != in {
		t.Errorf("empty profile (treated as off) mutated input: %q", got)
	}
}

func TestApplyDefaultMasksHighConfidenceSecrets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		masks []string // substrings that should be GONE after redaction
		keeps []string // substrings that should still be present
	}{
		{
			name:  "openai sk- token",
			in:    "Authorization: Bearer sk-proj-AbCdEfGh1234567890qwertyXYZ",
			masks: []string{"sk-proj-AbCdEfGh1234567890qwertyXYZ"},
			keeps: []string{"Authorization", "Bearer"},
		},
		{
			name:  "slack xoxb token",
			in:    "slack token: xoxb-1234567890-abcdefghij-LongAndPainful",
			masks: []string{"xoxb-1234567890-abcdefghij-LongAndPainful"},
			keeps: []string{"slack token"},
		},
		{
			name:  "AWS access key",
			in:    "aws_access_key_id = AKIAIOSFODNN7EXAMPLE",
			masks: []string{"AKIAIOSFODNN7EXAMPLE"},
			keeps: []string{"aws_access_key_id"},
		},
		{
			name:  "JWT shape",
			in:    "jwt = eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
			masks: []string{"eyJhbGciOiJIUzI1NiJ9"},
			keeps: []string{"jwt ="},
		},
		{
			name:  "email",
			in:    "contact: alice@example.com please",
			masks: []string{"alice@example.com"},
			keeps: []string{"contact:", "please"},
		},
		{
			name:  "GCP service-account private_key field",
			in:    `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----\n"}`,
			masks: []string{"BEGIN PRIVATE KEY"},
			keeps: []string{"service_account"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redact.Apply(tc.in, redact.ProfileDefault)
			for _, want := range tc.masks {
				if strings.Contains(out, want) {
					t.Errorf("default profile let %q through; got %q", want, out)
				}
			}
			for _, want := range tc.keeps {
				if !strings.Contains(out, want) {
					t.Errorf("default profile stripped legitimate context %q; got %q", want, out)
				}
			}
			if !strings.Contains(out, redact.Mask) {
				t.Errorf("expected the mask marker %q in output; got %q", redact.Mask, out)
			}
		})
	}
}

func TestApplyDefaultDoesNotMaskProse(t *testing.T) {
	t.Parallel()
	// Strings that LOOK suspicious but shouldn't trip the patterns.
	prose := []string{
		"sk-and-flowers please",                   // too short after sk-
		"Build hash 5fe8b9c0a01dfa3 looks OK",     // not credit-card length
		"port 12345-67890 is open",                // not an SSN
		"contact us via the form",                 // no email
		"discussion of AKIA notation in the spec", // word break
	}
	for _, in := range prose {
		t.Run(in, func(t *testing.T) {
			if out := redact.Apply(in, redact.ProfileDefault); out != in {
				t.Errorf("ProfileDefault redacted prose: in=%q out=%q", in, out)
			}
		})
	}
}

func TestApplyStrictAddsHighFPRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		masks []string
	}{
		// 4111-1111-1111-1111 passes Luhn.
		{name: "credit card (Luhn-valid)", in: "card: 4111-1111-1111-1111", masks: []string{"4111-1111-1111-1111"}},
		{name: "US SSN", in: "ssn 123-45-6789 here", masks: []string{"123-45-6789"}},
		{name: "E.164 phone", in: "call me +14155552671 tonight", masks: []string{"+14155552671"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redact.Apply(tc.in, redact.ProfileStrict)
			for _, want := range tc.masks {
				if strings.Contains(out, want) {
					t.Errorf("strict profile let %q through; got %q", want, out)
				}
			}
		})
	}
}

func TestApplyStrictLuhnRejectsRandomDigits(t *testing.T) {
	t.Parallel()
	// 13 digits that don't pass Luhn — must NOT be masked.
	in := "transaction id 1234567890123"
	if !strings.Contains(in, "1234567890123") {
		t.Fatal("test fixture broken")
	}
	out := redact.Apply(in, redact.ProfileStrict)
	if !strings.Contains(out, "1234567890123") {
		t.Errorf("strict profile false-positively masked a non-Luhn 13-digit number: %q -> %q", in, out)
	}
}

func TestApplyStrictAlsoAppliesDefault(t *testing.T) {
	t.Parallel()
	in := "key sk-AAAAAAAAAAAAAAAAAAAA and card 4111-1111-1111-1111"
	out := redact.Apply(in, redact.ProfileStrict)
	if strings.Contains(out, "sk-AAAAAAAAAAAAAAAAAAAA") {
		t.Errorf("strict didn't include default's sk-: %q", out)
	}
	if strings.Contains(out, "4111-1111-1111-1111") {
		t.Errorf("strict didn't mask the credit card: %q", out)
	}
}

func TestFuncReturnsNilForOff(t *testing.T) {
	t.Parallel()
	if f := redact.Func(redact.ProfileOff); f != nil {
		t.Errorf("Func(ProfileOff) returned non-nil; callers rely on nil to skip work")
	}
	if f := redact.Func(""); f != nil {
		t.Errorf("Func(\"\") returned non-nil")
	}
}

func TestFuncDefaultAndStrictWork(t *testing.T) {
	t.Parallel()
	dflt := redact.Func(redact.ProfileDefault)
	if dflt == nil {
		t.Fatal("Func(ProfileDefault) returned nil")
	}
	if got := dflt("contact alice@example.com"); strings.Contains(got, "alice@example.com") {
		t.Errorf("default redactor func didn't mask: %q", got)
	}

	strict := redact.Func(redact.ProfileStrict)
	if got := strict("ssn 123-45-6789"); strings.Contains(got, "123-45-6789") {
		t.Errorf("strict redactor func didn't mask SSN: %q", got)
	}
}

func TestValid(t *testing.T) {
	t.Parallel()
	for _, p := range []redact.Profile{redact.ProfileOff, redact.ProfileDefault, redact.ProfileStrict, ""} {
		if !redact.Valid(p) {
			t.Errorf("Valid(%q) = false, want true", p)
		}
	}
	for _, p := range []redact.Profile{"verbose", "loud", "yes please"} {
		if redact.Valid(p) {
			t.Errorf("Valid(%q) = true, want false", p)
		}
	}
}
