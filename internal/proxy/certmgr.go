package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// certManager holds a TLS keypair behind an atomic pointer so an in-flight
// TLS handshake can swap to a freshly-loaded cert without dropping the
// listener. Go's tls.Config.GetCertificate is called per ClientHello, so
// atomic.Pointer swaps are visible to the very next handshake — there is
// no window where a cert renewal forces a restart.
//
// Polling os.Stat ModTime is intentional: it stays correct under bind
// mounts, ConfigMap projected files (Kubernetes), and cert-manager
// renewals. fsnotify is louder, less portable, and not actually faster
// than a 30s poll for cert rotation.
type certManager struct {
	certPath string
	keyPath  string
	cert     atomic.Pointer[tls.Certificate]

	// modTimes tracks the last-seen ModTime for cert and key. A change in
	// either triggers a reload. We store both so a cert-only or key-only
	// touch (writers that touch files independently — cert-manager does
	// this) still fires.
	lastCertMod atomic.Int64
	lastKeyMod  atomic.Int64
}

// newCertManager builds an unloaded manager. Call Load before use.
func newCertManager(certPath, keyPath string) *certManager {
	return &certManager{certPath: certPath, keyPath: keyPath}
}

// Load reads cert + key from disk, parses them, and atomically installs
// the result. Idempotent and safe to call concurrently with Get.
//
// Two-file rotation hazard (C4): cert-manager and the Kubernetes
// ConfigMap projector rewrite cert and key as independent files. A
// naive `tls.LoadX509KeyPair` reads them in two separate syscalls and
// can splice old-cert / new-key (or vice versa) when a rotation lands
// between the reads. The spliced pair fails handshakes — at best — or
// silently presents the wrong identity if the keys happen to match.
//
// loadAtomicPair below addresses this with a read-then-restat barrier:
// any ModTime advance between the bytes-read and the re-stat is treated
// as a torn read and retried. `tls.X509KeyPair` also internally
// verifies public/private key consistency, so any inconsistency that
// slips past the ModTime check surfaces as a parse error.
func (m *certManager) Load(certPath, keyPath string) error {
	pair, certMod, keyMod, err := loadAtomicPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load keypair (%s, %s): %w", certPath, keyPath, err)
	}
	// LoadX509KeyPair does not populate Leaf; populate it ourselves so
	// downstream code (logging, tests) can read the parsed cert without
	// re-parsing on every handshake.
	if len(pair.Certificate) > 0 {
		leaf, parseErr := x509.ParseCertificate(pair.Certificate[0])
		if parseErr == nil {
			pair.Leaf = leaf
		}
	}
	m.cert.Store(&pair)
	m.lastCertMod.Store(certMod)
	m.lastKeyMod.Store(keyMod)
	return nil
}

// loadAtomicMaxAttempts caps the torn-read retry. Three attempts cover
// the worst realistic case (writer rewrites both files in two
// non-overlapping syscalls); beyond that, something pathological is
// happening on the filesystem and we should surface the error rather
// than spin.
const loadAtomicMaxAttempts = 3

// loadAtomicPair reads cert + key consistently. On each attempt:
//
//  1. Stat cert and key (record ModTime baseline).
//  2. Read cert bytes, then key bytes.
//  3. Re-stat cert and key.
//  4. If either ModTime advanced between (1) and (3), the bytes are
//     potentially torn — retry.
//  5. tls.X509KeyPair verifies pub/priv consistency; a "private key
//     does not match" error is the classic torn-read signature and
//     also triggers a retry.
//
// Returns the parsed keypair and the ModTimes-as-of-the-successful-read
// so callers can store them as the "last known good" baseline.
func loadAtomicPair(certPath, keyPath string) (tls.Certificate, int64, int64, error) {
	var lastErr error
	for attempt := 0; attempt < loadAtomicMaxAttempts; attempt++ {
		certStat0, err := os.Stat(certPath)
		if err != nil {
			return tls.Certificate{}, 0, 0, fmt.Errorf("stat cert: %w", err)
		}
		keyStat0, err := os.Stat(keyPath)
		if err != nil {
			return tls.Certificate{}, 0, 0, fmt.Errorf("stat key: %w", err)
		}
		certBytes, err := os.ReadFile(certPath)
		if err != nil {
			return tls.Certificate{}, 0, 0, fmt.Errorf("read cert: %w", err)
		}
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			return tls.Certificate{}, 0, 0, fmt.Errorf("read key: %w", err)
		}
		certStat1, err := os.Stat(certPath)
		if err != nil {
			return tls.Certificate{}, 0, 0, fmt.Errorf("re-stat cert: %w", err)
		}
		keyStat1, err := os.Stat(keyPath)
		if err != nil {
			return tls.Certificate{}, 0, 0, fmt.Errorf("re-stat key: %w", err)
		}
		if !certStat0.ModTime().Equal(certStat1.ModTime()) || !keyStat0.ModTime().Equal(keyStat1.ModTime()) {
			lastErr = errors.New("cert/key rotated mid-read; retrying")
			continue
		}
		pair, err := tls.X509KeyPair(certBytes, keyBytes)
		if err != nil {
			// X509KeyPair surfaces pub/priv mismatch via this exact
			// wording (see crypto/tls.X509KeyPair). Retry it; let
			// other PEM-parse errors propagate immediately since
			// they're persistent.
			if strings.Contains(err.Error(), "private key does not match public key") {
				lastErr = err
				continue
			}
			return tls.Certificate{}, 0, 0, fmt.Errorf("parse keypair: %w", err)
		}
		return pair, certStat1.ModTime().UnixNano(), keyStat1.ModTime().UnixNano(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("unknown torn-read failure")
	}
	return tls.Certificate{}, 0, 0, fmt.Errorf("consistent keypair load failed after %d attempts: %w", loadAtomicMaxAttempts, lastErr)
}

// Get implements tls.Config.GetCertificate. Returns the currently cached
// keypair. Never returns nil cert with nil error — if no keypair is
// loaded yet, returns an error so the handshake fails closed (better
// than serving a zero-value cert).
func (m *certManager) Get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := m.cert.Load()
	if c == nil {
		return nil, errors.New("certmgr: no certificate loaded")
	}
	return c, nil
}

// leafFingerprint returns the SHA-256 of the DER-encoded leaf cert (lower
// hex). Used for logging which cert is currently active so an operator
// rotating certs can confirm the swap took effect.
func (m *certManager) leafFingerprint() string {
	c := m.cert.Load()
	if c == nil || len(c.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// checkAndReload is the inner unit factored out of Watch so tests can
// drive one reload cycle synchronously. It compares current ModTime
// against the last-seen pair and, on change, reloads. Returns (reloaded,
// err). A failed reload keeps the previous cert active — we never
// substitute a broken cert for a working one.
func (m *certManager) checkAndReload(logger *slog.Logger) (bool, error) {
	certStat, err := os.Stat(m.certPath)
	if err != nil {
		return false, fmt.Errorf("stat cert %s: %w", m.certPath, err)
	}
	keyStat, err := os.Stat(m.keyPath)
	if err != nil {
		return false, fmt.Errorf("stat key %s: %w", m.keyPath, err)
	}
	curCertMod := certStat.ModTime().UnixNano()
	curKeyMod := keyStat.ModTime().UnixNano()
	if curCertMod == m.lastCertMod.Load() && curKeyMod == m.lastKeyMod.Load() {
		return false, nil
	}
	if err := m.Load(m.certPath, m.keyPath); err != nil {
		// Load failure does not advance lastMod — we want to retry on
		// the next tick. The old cert remains the served one.
		return false, err
	}
	if logger != nil {
		logger.Info("tls certificate reloaded",
			slog.String("cert_path", m.certPath),
			slog.String("leaf_sha256", m.leafFingerprint()),
		)
	}
	return true, nil
}

// Watch polls every interval and reloads when either file's ModTime has
// advanced. Returns when ctx is cancelled. interval <= 0 disables the
// watcher (Watch returns immediately) so operators can opt out without
// removing the cert paths.
func (m *certManager) Watch(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.checkAndReload(logger); err != nil && logger != nil {
				logger.Warn("tls certificate reload failed",
					slog.String("cert_path", m.certPath),
					slog.Any("err", err),
				)
			}
		}
	}
}
