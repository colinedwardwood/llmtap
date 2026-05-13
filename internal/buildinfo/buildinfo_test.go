package buildinfo

import "testing"

func TestIsUnset(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "dev", "unknown"} {
		if !isUnset(in) {
			t.Errorf("isUnset(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"v0.1.0", "main", "abcd1234", "2025-01-01"} {
		if isUnset(in) {
			t.Errorf("isUnset(%q) = true, want false", in)
		}
	}
}

// TestResolveDoesntPanicAndReturnsThree asserts the public API contract:
// Resolve always returns three non-nil strings, never panics. The
// values depend on how the test binary was built; we don't pin them.
func TestResolveDoesntPanicAndReturnsThree(t *testing.T) {
	t.Parallel()
	version, commit, date := Resolve()
	// Sanity: each one is at least one of: a real value, "dev",
	// "unknown", or the empty string (env may set it to "").
	for _, s := range []string{version, commit, date} {
		if s == "<nil>" {
			t.Errorf("Resolve returned %q, expected a real string", s)
		}
	}
}
