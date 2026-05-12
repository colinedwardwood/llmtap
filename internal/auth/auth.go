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
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

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
// NewVerifier; callers ask Verify(plain) and get a boolean.
//
// Performance: each Verify call runs argon2id against every stored
// hash until a match is found. For typical deployments (1-3 tokens)
// this is fine; large allow-lists should consider a different design.
type Verifier struct {
	// Each entry caches the *parsed* PHC fields so we don't re-parse
	// every request. The original encoded strings are not retained.
	allow []parsed
}

// NewVerifier parses every encoded hash up-front. Returns an error if
// any entry is malformed — fail-fast at startup is the right behaviour
// for an auth boundary.
func NewVerifier(encoded []string) (*Verifier, error) {
	v := &Verifier{allow: make([]parsed, 0, len(encoded))}
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

// Verify reports whether plain matches any allow-listed token. Empty
// plaintext is rejected outright — never authenticate a request that
// didn't present a credential.
func (v *Verifier) Verify(plain string) bool {
	if !v.Enabled() || plain == "" {
		return false
	}
	for _, p := range v.allow {
		candidate := argon2.IDKey([]byte(plain), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
		if subtle.ConstantTimeCompare(candidate, p.key) == 1 {
			return true
		}
	}
	return false
}
