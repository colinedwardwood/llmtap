package provider

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func mustHaveAttr(t *testing.T, attrs []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, a := range attrs {
		if string(a.Key) == key {
			if a.Value.AsString() != want {
				t.Errorf("attr %s = %q want %q", key, a.Value.AsString(), want)
			}
			return
		}
	}
	t.Errorf("attr %s missing", key)
}

func mustHaveAttrInt64(t *testing.T, attrs []attribute.KeyValue, key string, want int64) {
	t.Helper()
	for _, a := range attrs {
		if string(a.Key) == key {
			if a.Value.AsInt64() != want {
				t.Errorf("attr %s = %d want %d", key, a.Value.AsInt64(), want)
			}
			return
		}
	}
	t.Errorf("attr %s missing", key)
}
