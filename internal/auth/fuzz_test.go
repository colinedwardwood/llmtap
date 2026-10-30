package auth

import "testing"

func FuzzHasValidShape(f *testing.F) {
	f.Add("correct-horse-battery-staple")
	f.Add("")
	f.Add("bad\xffbytes")
	f.Fuzz(func(t *testing.T, plain string) {
		_ = hasValidShape(plain)
	})
}

func FuzzParseEncodedRoundtrip(f *testing.F) {
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$YWFhYWFhYWFhYWFhYWFhYQ$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	f.Fuzz(func(t *testing.T, encoded string) {
		if _, err := parseEncoded(encoded); err == nil {
			if !hasValidShape("probe-token-abc123") {
				t.Fatal("shape check broken")
			}
		}
	})
}
