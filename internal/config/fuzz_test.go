package config

import (
	"strings"
	"testing"
)

func FuzzParseTokensFile(f *testing.F) {
	f.Add("# comment\n$argon2id$v=19$m=65536,t=3,p=2$abc$def\n")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 16*1024 {
			t.Skip("oversized")
		}
		_, _ = parseTokensFile([]byte(raw))
	})
}

func FuzzValidateListenLoopback(f *testing.F) {
	f.Add("127.0.0.1:4000")
	f.Add(":4000")
	f.Fuzz(func(t *testing.T, listen string) {
		_ = isLoopbackAddr(listen)
		if strings.Contains(listen, "\x00") {
			t.Skip()
		}
	})
}
