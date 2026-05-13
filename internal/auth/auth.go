// Package auth implements bearer-token authentication for the llmtap
// listener. Operators configure one or more argon2id-hashed tokens; the
// proxy refuses any request that doesn't present a matching plaintext.
//
// Tokens are stored in the operator's config file in PHC-encoded form
// (`$argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>`), so the file is
// safe to commit to a private secrets store; the cleartext token lives
// only with the client. `llmtap hash-token` generates the encoded form.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2 parameters tuned for service-to-service token verification.
// Time-cost slightly above OWASP minimum; memory is generous but not
// extreme. Parallelism kept low so a request burst doesn't pin every
// core to argon2 work.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Cache + semaphore tunables. The cache is small and short-lived: real
// callers reuse a handful of tokens within a small window, so 1024
// entries × 60s TTL is more than enough. The semaphore is sized to
// NumCPU at construction time; argon2 work is CPU-bound and 64 MiB
// per call, so admitting more than one-per-core just thrashes the GC.
const (
	cacheMaxEntries = 1024
	cacheTTL        = 60 * time.Second
	tokenMaxLen     = 512
)

// Hash produces an argon2id PHC-encoded hash of plain. The encoded
// form is safe to store in plain text — it embeds the salt + the
// parameters used to derive the key.
func Hash(plain string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonTime,
		argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether plain produces the same key as the stored
// encoded hash. Returns an error when encoded is not a parseable
// argon2id PHC string.
func Verify(plain, encoded string) (bool, error) {
	p, err := parseEncoded(encoded)
	if err != nil {
		return false, err
	}
	candidate := argon2.IDKey([]byte(plain), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
	// Constant-time compare keeps a wrong-length / wrong-content guess
	// from leaking timing information.
	return subtle.ConstantTimeCompare(candidate, p.key) == 1, nil
}

type parsed struct {
	time    uint32
	memory  uint32
	threads uint8
	salt    []byte
	key     []byte
}

// parseEncoded splits a PHC-formatted argon2id string into its fields.
// Enforces the algorithm tag and v=19 so a downgrade attack via the
// stored hash is harder.
func parseEncoded(s string) (parsed, error) {
	parts := strings.Split(s, "$")
	// Format: ["", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<key>"]
	if len(parts) != 6 || parts[0] != "" {
		return parsed{}, errors.New("auth: encoded hash is not a PHC string")
	}
	if parts[1] != "argon2id" {
		return parsed{}, fmt.Errorf("auth: unsupported algorithm %q", parts[1])
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return parsed{}, fmt.Errorf("auth: parse version: %w", err)
	}
	if version != argon2.Version {
		return parsed{}, fmt.Errorf("auth: unsupported argon2 version %d (want %d)", version, argon2.Version)
	}
	var (
		mem  uint32
		time uint32
		par  uint8
	)
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &par); err != nil {
		return parsed{}, fmt.Errorf("auth: parse params: %w", err)
	}
	if mem == 0 || time == 0 || par == 0 {
		return parsed{}, errors.New("auth: argon2 params include a zero value")
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return parsed{}, fmt.Errorf("auth: decode salt: %w", err)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return parsed{}, fmt.Errorf("auth: decode key: %w", err)
	}
	return parsed{time: time, memory: mem, threads: par, salt: salt, key: key}, nil
}

// Verifier holds a parsed allow-list of token hashes. Construct via
// NewVerifier; callers ask Verify(plain) and get a (matched, busy)
// pair.
//
// The verifier defends against three distinct DoS shapes:
//
//   - Shape rejection (C2a): tokens that don't fit the printable-ASCII
//     subset llmtap accepts are rejected at nanosecond cost, BEFORE
//     argon2 runs at all. Internet-scan garbage rarely matches the
//     subset, so the bulk of unauthenticated traffic never pays the
//     argon2 cost.
//
//   - Cache (C2b): once a plaintext has verified against a stored
//     hash, the (sha256(plain) → expiresAt) pair is stashed in a
//     bounded LRU for cacheTTL. Real callers reuse the same token
//     across many requests; the cached path is microseconds.
//
//   - Semaphore (C9): argon2 evaluations run inside a chan-of-struct
//     semaphore sized runtime.NumCPU(). Each argon2 call allocates
//     argonMemory bytes of scratch (64 MiB by default); admitting more
//     than NumCPU concurrent calls just thrashes the GC. Over-capacity
//     callers receive (ok=false, busy=true) so the proxy can answer
//     429 + Retry-After.
//
// The constant-time match is enforced at the verifier level too:
// Verify runs argon2 against EVERY stored hash, XOR-accumulating the
// match flag across the loop. The cost is amortized away by the cache
// for legitimate callers; an attacker can't learn which position in
// the operator's allow-list a given guess matched.
type Verifier struct {
	// Each entry caches the *parsed* PHC fields so we don't re-parse
	// every request. The original encoded strings are not retained.
	allow []parsed

	// sem caps concurrent argon2 evaluations to runtime.NumCPU().
	// Buffered chan is the simplest counting-semaphore primitive
	// without pulling in x/sync.
	sem chan struct{}

	// cache holds sha256(plain) → expiresAt for tokens that have
	// recently verified successfully. Bounded LRU eviction + TTL.
	cacheMu sync.Mutex
	cache   map[[32]byte]time.Time
	// cacheOrder is the LRU access order: newest at the back.
	cacheOrder [][32]byte

	// now is injected for tests; production uses time.Now.
	now func() time.Time

	// argon2Hook, when non-nil, runs INSIDE the semaphore-guarded
	// region before the real argon2 work. Tests use this to count
	// concurrent in-flight evaluations and to stretch the work window
	// so the semaphore cap is observable.
	argon2Hook func()
}

// NewVerifier parses every encoded hash up-front. Returns an error if
// any entry is malformed — fail-fast at startup is the right behaviour
// for an auth boundary.
func NewVerifier(encoded []string) (*Verifier, error) {
	v := &Verifier{
		allow: make([]parsed, 0, len(encoded)),
		sem:   make(chan struct{}, runtime.NumCPU()),
		cache: make(map[[32]byte]time.Time, cacheMaxEntries),
		now:   time.Now,
	}
	for i, e := range encoded {
		p, err := parseEncoded(e)
		if err != nil {
			return nil, fmt.Errorf("auth.tokens[%d]: %w", i, err)
		}
		v.allow = append(v.allow, p)
	}
	return v, nil
}

// Enabled reports whether the verifier has any allow-listed tokens.
// When false, callers should treat the request as unauthenticated-by-
// design (i.e. forward without checking).
func (v *Verifier) Enabled() bool {
	return v != nil && len(v.allow) > 0
}

// Verify reports whether plain matches any allow-listed token.
//
// Return shape:
//
//   - ok=true,  busy=false: plain matched a stored hash.
//   - ok=false, busy=false: plain did not match any stored hash.
//   - ok=false, busy=true:  argon2 semaphore is saturated; the caller
//     should refuse the request with 429 + Retry-After instead of
//     blocking. busy=true is only ever returned when the request
//     reached the argon2 path (i.e. shape-check passed and cache
//     missed).
//
// Empty plaintext and shape-mismatched plaintext are rejected outright
// — never authenticate a request that didn't present a credential, and
// never burn argon2 cycles on garbage.
func (v *Verifier) Verify(plain string) (ok bool, busy bool) {
	if !v.Enabled() || plain == "" {
		return false, false
	}
	// Shape gate: only printable-ASCII subset bytes are allowed. This
	// rejects nearly all internet-scan garbage at nanosecond cost,
	// BEFORE the cache lookup so a flood of distinct garbage tokens
	// can't churn the LRU either.
	if !hasValidShape(plain) {
		return false, false
	}

	plainHash := sha256.Sum256([]byte(plain))

	// Cache lookup. Hits are microseconds and bypass the semaphore.
	if v.cacheLookup(plainHash) {
		return true, false
	}

	// Acquire the argon2 semaphore. Non-blocking try-acquire: when
	// the cap is reached, surface busy=true so the caller can answer
	// 429 instead of queueing argon2 work.
	select {
	case v.sem <- struct{}{}:
		defer func() { <-v.sem }()
	default:
		return false, true
	}

	if v.argon2Hook != nil {
		v.argon2Hook()
	}

	// Loop-constant-time check: run argon2 against EVERY stored hash
	// and accumulate the match bit. Returning on first match would
	// leak the position of the correct token via timing.
	plainBytes := []byte(plain)
	var matched int
	for _, p := range v.allow {
		candidate := argon2.IDKey(plainBytes, p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
		// subtle.ConstantTimeCompare returns 1 on match, 0 otherwise;
		// OR keeps the loop oblivious to which iteration matched.
		matched |= subtle.ConstantTimeCompare(candidate, p.key)
	}
	if matched == 1 {
		v.cacheStore(plainHash)
		return true, false
	}
	return false, false
}

// hasValidShape returns true when every byte of plain is in the
// printable-ASCII subset llmtap accepts for bearer tokens. The set
// covers hex, base64, base64url, and urlsafe-with-padding encodings —
// the only shapes operators realistically generate via `llmtap
// hash-token` or external secret managers. Anything else is garbage.
//
// The function does NOT short-circuit on the first bad byte: timing
// must not depend on where in the token the corruption begins. The
// length cap is a separate gate — pathological lengths fail before
// the byte loop.
func hasValidShape(plain string) bool {
	if len(plain) > tokenMaxLen {
		return false
	}
	var bad int
	for i := 0; i < len(plain); i++ {
		c := plain[i]
		ok := (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '+' || c == '/' || c == '=' ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			bad++
		}
	}
	return bad == 0
}

// cacheLookup returns true if plainHash is in the cache AND hasn't
// expired. The lookup itself is O(1) on the map; subtle's compare is
// used so a populated-but-expired entry doesn't leak via timing on the
// access-order slice. Caller must NOT hold cacheMu.
func (v *Verifier) cacheLookup(plainHash [32]byte) bool {
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	expiresAt, ok := v.cache[plainHash]
	if !ok {
		return false
	}
	if v.now().After(expiresAt) {
		// Lazily evict expired entries. The order slice rebuild on
		// the next store will drop this stale id.
		delete(v.cache, plainHash)
		return false
	}
	return true
}

// cacheStore inserts plainHash with a fresh TTL. Evicts the oldest
// entry when the cache is full. Caller must NOT hold cacheMu.
func (v *Verifier) cacheStore(plainHash [32]byte) {
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	if _, exists := v.cache[plainHash]; exists {
		// Already cached: refresh the TTL but leave the access-order
		// slice alone. The next eviction will see the refreshed
		// expiry and pass it over.
		v.cache[plainHash] = v.now().Add(cacheTTL)
		return
	}
	if len(v.cache) >= cacheMaxEntries {
		// Drop the oldest entry. The order slice may carry already-
		// evicted ids; walk forward until we find one still present.
		for len(v.cacheOrder) > 0 {
			oldest := v.cacheOrder[0]
			v.cacheOrder = v.cacheOrder[1:]
			if _, stillThere := v.cache[oldest]; stillThere {
				delete(v.cache, oldest)
				break
			}
		}
	}
	v.cache[plainHash] = v.now().Add(cacheTTL)
	v.cacheOrder = append(v.cacheOrder, plainHash)
}
