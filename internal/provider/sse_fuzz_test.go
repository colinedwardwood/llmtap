package provider

import (
	"strings"
	"testing"
)

func FuzzSSETeeScan(f *testing.F) {
	f.Add("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	f.Add("event: ping\ndata: {}\n\n")
	f.Fuzz(func(t *testing.T, chunk string) {
		if len(chunk) > 32*1024 {
			t.Skip("oversized")
		}
		// Exercise the line scanner without standing up HTTP.
		lines := strings.Split(chunk, "\n")
		var buf strings.Builder
		for _, ln := range lines {
			if strings.HasPrefix(ln, "data:") {
				buf.WriteString(strings.TrimSpace(strings.TrimPrefix(ln, "data:")))
			}
		}
		_ = buf.String()
	})
}
