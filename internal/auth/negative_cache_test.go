package auth_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/auth"
)

func TestVerifierNegativeCacheSkipsArgon2(t *testing.T) {
	t.Parallel()
	h, err := auth.Hash("good-token")
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier([]string{h})
	if err != nil {
		t.Fatal(err)
	}
	var argonCalls atomic.Int32
	auth.SetArgon2HookForTest(v, func() { argonCalls.Add(1) })
	auth.SetNowForTest(v, func() time.Time { return time.Unix(0, 0) })

	if ok, busy := v.Verify("bad-token"); ok || busy {
		t.Fatalf("first bad verify = ok=%v busy=%v", ok, busy)
	}
	if argonCalls.Load() != 1 {
		t.Fatalf("argon2 calls = %d, want 1", argonCalls.Load())
	}
	if ok, busy := v.Verify("bad-token"); ok || busy {
		t.Fatalf("second bad verify = ok=%v busy=%v", ok, busy)
	}
	if argonCalls.Load() != 1 {
		t.Fatalf("negative cache should skip argon2, calls = %d", argonCalls.Load())
	}
}

func TestVerifierMaxAllowListRejected(t *testing.T) {
	t.Parallel()
	hashes := make([]string, auth.MaxAllowListTokens+1)
	for i := range hashes {
		h, err := auth.Hash("tok")
		if err != nil {
			t.Fatal(err)
		}
		hashes[i] = h
	}
	if _, err := auth.NewVerifier(hashes); err == nil {
		t.Fatal("expected allow-list ceiling error")
	}
}
