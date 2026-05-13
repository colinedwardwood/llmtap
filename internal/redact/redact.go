// Package redact masks sensitive substrings inside captured prompt and
// completion content before that content reaches a span attribute or
// the otelslog bridge.
//
// llmtap's privacy story has long been "content capture is off by
// default; pair it with collector-side redaction when you turn it on".
// That is not a credible offer to operators whose first deployment is
// a few servers behind one collector — they can opt into events for
// debugging, only to discover six weeks later that an `sk-...` ended
// up in a prompt the model ignored. The proxy is the right place to
// scrub: it sees content first and has bounded surface to test.
package redact

import (
	"regexp"
	"strings"
)

// Profile names the redaction policy applied to captured content.
type Profile string

const (
	// ProfileOff disables redaction. Content reaches span attributes
	// raw — for operators who explicitly want unfiltered visibility
	// and accept the privacy cost.
	ProfileOff Profile = "off"

	// ProfileDefault is the recommended policy. Masks high-confidence
	// credential and PII patterns that show up in real LLM traffic:
	// API key prefixes (OpenAI sk-, Slack xox*, AWS AKIA), GCP
	// service-account JSON markers, common JWT shape, RFC-5322
	// emails.
	ProfileDefault Profile = "default"

	// ProfileStrict adds higher-false-positive PII patterns on top
	// of ProfileDefault: credit-card Luhn-shaped sequences, US SSN,
	// E.164 phone numbers. For regulated workloads.
	ProfileStrict Profile = "strict"
)

// Mask is the replacement string used in place of detected secrets.
// Kept short so spans aren't padded; "[REDACTED]" is recognizable
// when scrolling a trace.
const Mask = "[REDACTED]"

// Apply replaces every match of the profile's pattern set with Mask.
// Unknown profiles fall back to identity (no-op) — callers should
// validate at config time and never reach the default arm here.
func Apply(s string, profile Profile) string {
	switch profile {
	case ProfileOff, "":
		return s
	case ProfileDefault:
		return applyAll(s, defaultPatterns)
	case ProfileStrict:
		s = applyAll(s, defaultPatterns)
		return applyAll(s, strictPatterns)
	default:
		return s
	}
}

// Func returns a redaction closure bound to a profile. Use it when
// passing the redactor across an API boundary that wants a `func(string)
// string` shape instead of (string, Profile) → string.
//
// Returns nil for ProfileOff so callers can cheaply skip work via a
// nil check; non-nil for every other profile, so the gate is "if r !=
// nil { s = r(s) }".
func Func(profile Profile) func(string) string {
	if profile == ProfileOff || profile == "" {
		return nil
	}
	return func(s string) string { return Apply(s, profile) }
}

// Valid reports whether p is one of the known profiles. Config
// validation should refuse unknown values rather than degrade.
func Valid(p Profile) bool {
	switch p {
	case ProfileOff, ProfileDefault, ProfileStrict, "":
		return true
	}
	return false
}

// replacer is the shape applyAll consumes. *regexp.Regexp satisfies
// it natively; *luhnReplacer wraps a regexp with a Luhn check.
type replacer interface {
	ReplaceAllString(s, repl string) string
}

// applyAll runs each pattern against s and replaces every match with
// Mask. Patterns are anchored loosely on purpose — they look for the
// distinguishing prefix or character set, not the whole structural
// envelope. False negatives are preferred over false positives that
// would mangle prompt text.
func applyAll(s string, pats []replacer) string {
	for _, p := range pats {
		s = p.ReplaceAllString(s, Mask)
	}
	return s
}

var (
	// defaultPatterns: high-confidence credential / PII markers.
	defaultPatterns = []replacer{
		// OpenAI / Stripe-style "sk-" keys: at least 20 chars of
		// [A-Za-z0-9_-] after the prefix. The 20-char floor cuts
		// down on false positives from prose like "sk-and-flowers".
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
		// Slack bot / user tokens.
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
		// AWS access key ID.
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		// GCP service-account JSON: the "private_key" field is a
		// fingerprint. Match conservatively on the JSON key shape.
		regexp.MustCompile(`"private_key"\s*:\s*"[^"]+"`),
		// Common JWT: three base64url segments separated by dots.
		// Length floor avoids matching random "a.b.c" substrings.
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
		// RFC-5322-ish email. Strict spec is overkill for redaction;
		// this catches the common cases without trying to parse the
		// long tail.
		regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
	}

	// strictPatterns: high-false-positive PII. Layered on top of
	// defaultPatterns only when explicitly requested.
	strictPatterns = []replacer{
		// Credit-card-shaped (13-19 digits, dashes / spaces allowed).
		// Luhn check is applied post-match; non-Luhn-valid hits stay
		// in the text.
		luhnSafeCC,
		// US SSN.
		regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
		// E.164 phone — +<country><10–15 digits>.
		regexp.MustCompile(`\+\d{8,15}\b`),
	}
)

// luhnSafeCC is a custom replacer rather than a plain regexp because
// 13-19-digit sequences are common in real prose (port numbers,
// transaction IDs, build hashes). We only mask numbers that pass the
// Luhn check, dramatically cutting false positives.
var luhnSafeCC = &luhnReplacer{
	re: regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`),
}

type luhnReplacer struct {
	re *regexp.Regexp
}

// ReplaceAllString satisfies the same surface as *regexp.Regexp so
// applyAll can treat it uniformly.
func (l *luhnReplacer) ReplaceAllString(s, repl string) string {
	return l.re.ReplaceAllStringFunc(s, func(match string) string {
		digits := stripNonDigits(match)
		if len(digits) < 13 || len(digits) > 19 {
			return match
		}
		if !luhn(digits) {
			return match
		}
		return repl
	})
}

func stripNonDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhn returns true if the digit string passes the Luhn checksum.
func luhn(digits string) bool {
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}
