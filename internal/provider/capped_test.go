package provider

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCappedBuilderStopsAtLimit(t *testing.T) {
	t.Parallel()
	var b cappedBuilder
	chunk := strings.Repeat("a", 4096)
	for range 32 { // 128 KiB attempted, twice the cap
		b.WriteString(chunk)
	}
	if b.Len() != maxAssembledBytes {
		t.Fatalf("Len() = %d, want exactly %d", b.Len(), maxAssembledBytes)
	}
	if !b.Truncated() {
		t.Fatal("expected Truncated() = true after overflowing the cap")
	}
}

func TestCappedBuilderUnderLimitNotTruncated(t *testing.T) {
	t.Parallel()
	var b cappedBuilder
	b.WriteString("hello world")
	if b.Truncated() {
		t.Fatal("small write must not report truncation")
	}
	if b.String() != "hello world" {
		t.Fatalf("String() = %q", b.String())
	}
}

func TestCappedBuilderDoesNotSplitRunes(t *testing.T) {
	t.Parallel()
	var b cappedBuilder
	// Fill to 1 byte below the cap, then write a 3-byte rune that
	// straddles the boundary; it must be dropped whole.
	b.WriteString(strings.Repeat("x", maxAssembledBytes-1))
	b.WriteString("\u65e5\u672c") // 3-byte CJK runes
	if !utf8.ValidString(b.String()) {
		t.Fatal("capped output is not valid UTF-8")
	}
	if !b.Truncated() {
		t.Fatal("boundary-straddling write must set truncated")
	}
}
