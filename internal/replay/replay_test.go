package replay

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRecorderRedactsAndLoads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "recording.jsonl")
	rec := NewRecorder(path, func(s string) string {
		return strings.ReplaceAll(s, "secret", "[REDACTED]")
	})
	if err := rec.Append(Entry{
		Method: "POST", Path: "/v1/responses?key=secret",
		Request: `{"token":"secret"}`, Response: `{"text":"secret"}`,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	got := entries[0]
	if strings.Contains(got.Path+got.Request+got.Response, "secret") {
		t.Fatalf("recording leaked secret: %+v", got)
	}
}

func TestDiffCanonicalJSON(t *testing.T) {
	t.Parallel()
	left := []Entry{{Status: 200, Response: `{"a":1,"b":2}`}}
	right := []Entry{{Status: 200, Response: `{ "b": 2, "a": 1 }`}}
	if got := Diff(left, right); len(got) != 0 {
		t.Fatalf("diff = %+v", got)
	}
	right[0].Response = `{"a":2}`
	if got := Diff(left, right); len(got) != 1 || got[0].Field != "response" {
		t.Fatalf("diff = %+v", got)
	}
}
