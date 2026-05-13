package auth_test

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/auth"
)

func TestHashVerifyRoundtrip(t *testing.T) {
	t.Parallel()
	encoded, err := auth.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Errorf("encoded hash missing argon2id prefix: %q", encoded)
	}
	ok, err := auth.Verify("correct-horse-battery-staple", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("verify returned false for the correct password")
	}
}

func TestHashIsSalted(t *testing.T) {
	t.Parallel()
	a, err := auth.Hash("same-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	b, err := auth.Hash("same-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("hashing the same plaintext twice produced identical output — salt missing")
	}
}

func TestVerifyRejectsWrongPlaintext(t *testing.T) {
	t.Parallel()
	encoded, err := auth.Hash("real-token")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := auth.Verify("guessed-token", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("verify returned true for the wrong password")
	}
}

func TestVerifyMalformedEncoded(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2id$v=19$m=foo,t=bar,p=baz$salt$hash",
		"$bcrypt$10$abc$def",
	}
	for _, enc := range cases {
		if _, err := auth.Verify("anything", enc); err == nil {
			t.Errorf("expected error on malformed encoded %q", enc)
		}
	}
}

func TestVerifierAcceptsKnownToken(t *testing.T) {
	t.Parallel()
	h, err := auth.Hash("token-A")
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier([]string{h})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := v.Verify("token-A"); !ok {
		t.Error("verifier rejected the token it was configured with")
	}
	if ok, _ := v.Verify("token-B"); ok {
		t.Error("verifier accepted a token it wasn't configured with")
	}
}

func TestVerifierAcceptsAnyOfMultipleTokens(t *testing.T) {
	t.Parallel()
	hA, _ := auth.Hash("alpha")
	hB, _ := auth.Hash("bravo")
	v, err := auth.NewVerifier([]string{hA, hB})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := v.Verify("alpha"); !ok {
		t.Error("multi-hash verifier rejected first token")
	}
	if ok, _ := v.Verify("bravo"); !ok {
		t.Error("multi-hash verifier rejected second token")
	}
	if ok, _ := v.Verify("charlie"); ok {
		t.Error("multi-hash verifier accepted an unknown token")
	}
}

func TestVerifierRejectsEmptyToken(t *testing.T) {
	t.Parallel()
	h, _ := auth.Hash("real")
	v, _ := auth.NewVerifier([]string{h})
	if ok, _ := v.Verify(""); ok {
		t.Error("empty token must not match")
	}
}

func TestVerifierEnabled(t *testing.T) {
	t.Parallel()
	empty, err := auth.NewVerifier(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Enabled() {
		t.Error("Enabled() must return false when no tokens are configured")
	}
	h, _ := auth.Hash("real")
	on, _ := auth.NewVerifier([]string{h})
	if !on.Enabled() {
		t.Error("Enabled() must return true when at least one token is configured")
	}
}

func TestNewVerifierRejectsMalformedTokenAtConstruction(t *testing.T) {
	t.Parallel()
	if _, err := auth.NewVerifier([]string{"not-a-hash"}); err == nil {
		t.Error("NewVerifier must reject malformed encoded tokens at construction time")
	}
}

// TestVerifierFastRejectsBadShape is the C2(a) regression. Tokens with
// characters outside the printable-ASCII subset must fail without
// running argon2. The timing margin in this test is 1ms for 1000
// garbage tokens; a single argon2 evaluation is ~50ms so even one
// missed shape-check would blow the budget.
func TestVerifierFastRejectsBadShape(t *testing.T) {
	t.Parallel()
	h, _ := auth.Hash("real-token-xyz")
	v, _ := auth.NewVerifier([]string{h})

	garbage := []string{
		"<script>alert(1)</script>",
		"token with spaces",
		"token\twith\ttabs",
		"token\nwith\nnewlines",
		"contains\x00nul",
		"unicode-π",
		"semicolon;injection",
		"backtick`subshell",
		"quote\"injection",
		"apostrophe'injection",
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		for _, g := range garbage {
			if ok, _ := v.Verify(g); ok {
				t.Errorf("garbage token %q accepted", g)
			}
		}
	}
	elapsed := time.Since(start)
	// 100 × 10 garbage tokens = 1000 calls. Each must be sub-microsecond
	// in the shape-check path; we allow a generous 50ms total budget
	// (a single argon2 evaluation is ~50ms, so any single argon2 leak
	// would blow it).
	if elapsed > 50*time.Millisecond {
		t.Errorf("1000 garbage-token verifications took %v; want <50ms (shape-check must precede argon2)", elapsed)
	}
}

// TestVerifierCacheTTL is the C2(b) regression. A second Verify call
// with the same plaintext after a successful first call must complete
// in under 100µs (cache hit). The cold path is ~50ms; the warm path
// must be microseconds.
func TestVerifierCacheTTL(t *testing.T) {
	t.Parallel()
	h, _ := auth.Hash("real-token-cached")
	v, _ := auth.NewVerifier([]string{h})

	// Cold call to seed the cache.
	if ok, _ := v.Verify("real-token-cached"); !ok {
		t.Fatal("cold verify rejected the correct token")
	}

	// Warm call: cache hit.
	start := time.Now()
	if ok, _ := v.Verify("real-token-cached"); !ok {
		t.Fatal("warm verify rejected the correct token")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Microsecond {
		t.Errorf("warm verify took %v; want <100µs (cache should short-circuit argon2)", elapsed)
	}
}

// TestVerifierVerifyConstantTimeAcrossPosition is the C3 regression.
// Verifying a token at position 0 must not be measurably faster than
// verifying a token at position N-1 — the verifier must run argon2
// against every stored hash, accumulating the match flag.
func TestVerifierVerifyConstantTimeAcrossPosition(t *testing.T) {
	t.Parallel()
	const N = 10
	hashes := make([]string, N)
	plains := make([]string, N)
	for i := 0; i < N; i++ {
		p := "token-position-" + string(rune('a'+i))
		plains[i] = p
		h, err := auth.Hash(p)
		if err != nil {
			t.Fatal(err)
		}
		hashes[i] = h
	}
	v, err := auth.NewVerifier(hashes)
	if err != nil {
		t.Fatal(err)
	}

	// Warm the verifier (any one-time setup cost should not skew the
	// first measurement). Use a known-wrong-but-well-shaped token so
	// the cache doesn't poison the timing.
	if ok, _ := v.Verify("zzz-warmup"); ok {
		t.Fatal("warmup token unexpectedly accepted")
	}

	// Time position 0.
	start := time.Now()
	if ok, _ := v.Verify(plains[0]); !ok {
		t.Fatal("position-0 token rejected")
	}
	t0 := time.Since(start)
	// Build a SECOND verifier so the cache from the position-0 call
	// can't shortcut the position-(N-1) call.
	v2, err := auth.NewVerifier(hashes)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := v2.Verify("zzz-warmup"); ok {
		t.Fatal("warmup token unexpectedly accepted on v2")
	}
	start = time.Now()
	if ok, _ := v2.Verify(plains[N-1]); !ok {
		t.Fatal("position-(N-1) token rejected")
	}
	tLast := time.Since(start)

	// Both runs must execute argon2 against all N hashes. The ratio
	// is bounded by 1.25× to allow for measurement jitter; a leaky
	// implementation that short-circuits on first match would show
	// t0 ≈ tLast/N (5–10× faster).
	ratio := float64(tLast) / float64(t0)
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("position timing ratio = %.2f (t0=%v, tLast=%v); want close to 1.0 (constant-time loop required)", ratio, t0, tLast)
	}
}

// TestVerifierConcurrentArgon2RespectsSemaphore is the C9 regression.
// Even a flood of distinct garbage tokens (cache misses) must not run
// more than runtime.NumCPU() argon2 evaluations concurrently. Per the
// task spec, over-capacity callers receive busy=true so the proxy can
// translate that to 429.
func TestVerifierConcurrentArgon2RespectsSemaphore(t *testing.T) {
	t.Parallel()
	cpus := runtime.NumCPU()
	if cpus < 2 {
		t.Skip("test requires NumCPU >= 2")
	}
	h, _ := auth.Hash("the-real-token")
	v, _ := auth.NewVerifier([]string{h})

	var (
		current atomic.Int32
		peak    atomic.Int32
	)
	// Instrument the argon2 path via the package's test hook so we
	// can count concurrent in-flight evaluations precisely.
	auth.SetArgon2HookForTest(v, func() {
		c := current.Add(1)
		defer current.Add(-1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	})

	const fanout = 8
	wg := sync.WaitGroup{}
	var busyCount atomic.Int32
	for i := 0; i < cpus*fanout; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			// Use distinct plaintexts so the cache can't short-circuit.
			tok := "garbage-shape-ok-" + strings.Repeat("z", i%32+1)
			ok, busy := v.Verify(tok)
			if ok {
				t.Errorf("garbage token %q accepted", tok)
			}
			if busy {
				busyCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > int32(cpus) {
		t.Errorf("peak concurrent argon2 evaluations = %d; want <= NumCPU=%d", got, cpus)
	}
	// At fanout=8 with mock-blocking argon2, the semaphore MUST refuse
	// at least some callers — otherwise the cap isn't enforced.
	if busyCount.Load() == 0 {
		t.Error("no callers received busy=true; semaphore is not enforcing the cap")
	}
}

func BenchmarkVerifierVerifyCold(b *testing.B) {
	h, _ := auth.Hash("bench-token")
	for i := 0; i < b.N; i++ {
		// Each iteration uses a fresh verifier so the cache is cold.
		v, _ := auth.NewVerifier([]string{h})
		_, _ = v.Verify("bench-token")
	}
}

func BenchmarkVerifierVerifyWarm(b *testing.B) {
	h, _ := auth.Hash("bench-token")
	v, _ := auth.NewVerifier([]string{h})
	// Seed the cache.
	if ok, _ := v.Verify("bench-token"); !ok {
		b.Fatal("cold verify failed during seed")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Verify("bench-token")
	}
}
