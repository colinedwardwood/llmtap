package redact

import "testing"

func FuzzRedactIdempotent(f *testing.F) {
	f.Add("hello sk-abcdefghijklmnopqrstuvwxyz1234567890 world")
	f.Add("")
	f.Add("-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----")
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 64*1024 {
			t.Skip("oversized input")
		}
		fn := Func(ProfileDefault)
		once := fn(in)
		twice := fn(once)
		if once != twice {
			t.Fatalf("redact not idempotent:\n%q\n!=\n%q", once, twice)
		}
	})
}
