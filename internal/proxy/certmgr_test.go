package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// writeSelfSignedCertKey writes a freshly minted leaf cert + matching
// key (P-256) into dir/cert-<sub>.pem and dir/key-<sub>.pem. Returns the
// paths and the SHA-256 fingerprint of the DER leaf so callers can pin
// "the cert currently on disk is exactly this one" assertions across
// reload cycles.
func writeSelfSignedCertKey(t *testing.T, sub string) (certPath, keyPath, spkiSHA string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: sub},
		DNSNames:              []string{"localhost", sub},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert-"+sub+".pem")
	keyPath = filepath.Join(dir, "key-"+sub+".pem")

	cf, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if encErr := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); encErr != nil {
		t.Fatal(encErr)
	}
	_ = cf.Close()

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	kf, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if encErr := pem.Encode(kf, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); encErr != nil {
		t.Fatal(encErr)
	}
	_ = kf.Close()

	sum := sha256.Sum256(der)
	spkiSHA = hex.EncodeToString(sum[:])
	return certPath, keyPath, spkiSHA
}

// movePEM rewrites destCert/destKey from srcCert/srcKey while preserving
// the destination paths — i.e. the same file paths the cert manager was
// constructed with. ModTime is bumped explicitly so we don't depend on
// filesystem clock granularity for the change-detection.
func movePEM(t *testing.T, srcCert, srcKey, destCert, destKey string) {
	t.Helper()
	cb, err := os.ReadFile(srcCert)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := os.ReadFile(srcKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destCert, cb, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destKey, kb, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(destCert, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(destKey, future, future); err != nil {
		t.Fatal(err)
	}
}

// TestCertManagerReloadsOnModTimeChange is the regression test for A23:
// when the cert/key files on disk are updated in place (cert-manager
// renewal pattern), the manager must observe the ModTime change on its
// next poll and atomically swap the served cert. Before the fix
// ServeTLS loaded the cert once and the renewal sat unread on disk
// until restart.
func TestCertManagerReloadsOnModTimeChange(t *testing.T) {
	t.Parallel()

	// Stage v1 — the cert the manager is constructed with.
	cert1, key1, sha1 := writeSelfSignedCertKey(t, "v1")
	// Stage v2 elsewhere, then copy v2 contents over the v1 paths so
	// the manager sees its OWN paths change ModTime (which is what
	// cert-manager renewal looks like in practice).
	cert2, key2, sha2 := writeSelfSignedCertKey(t, "v2")
	if sha1 == sha2 {
		t.Fatal("test setup: v1 and v2 fingerprints collided")
	}

	mgr := newCertManager(cert1, key1)
	if err := mgr.Load(cert1, key1); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if got := mgr.leafFingerprint(); got != sha1 {
		t.Fatalf("initial fingerprint = %s, want %s", got, sha1)
	}

	// First poll should be a no-op — files have not changed.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reloaded, err := mgr.checkAndReload(logger)
	if err != nil {
		t.Fatalf("no-op reload: %v", err)
	}
	if reloaded {
		t.Fatal("checkAndReload claimed a reload despite no ModTime change")
	}

	// Overwrite v1's paths with v2's contents and bump ModTime.
	movePEM(t, cert2, key2, cert1, key1)

	reloaded, err = mgr.checkAndReload(logger)
	if err != nil {
		t.Fatalf("reload after change: %v", err)
	}
	if !reloaded {
		t.Fatal("checkAndReload returned false despite ModTime change")
	}
	if got := mgr.leafFingerprint(); got != sha2 {
		t.Fatalf("post-reload fingerprint = %s, want %s", got, sha2)
	}

	// Confirm Get returns the new cert (live handshake path).
	got, err := mgr.Get(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Certificate) == 0 {
		t.Fatal("Get returned cert with no DER bytes")
	}
	gotSum := sha256.Sum256(got.Certificate[0])
	if hex.EncodeToString(gotSum[:]) != sha2 {
		t.Fatal("Get returned a stale cert after reload")
	}
}

// TestCertManagerLoadFailureKeepsPreviousCert asserts that a corrupted
// or partially-written file on disk does NOT clobber the in-memory cert.
// Cert-manager writes can race against our poll; the safe behaviour is
// to keep serving v1 and retry on the next tick.
func TestCertManagerLoadFailureKeepsPreviousCert(t *testing.T) {
	t.Parallel()

	cert1, key1, sha1 := writeSelfSignedCertKey(t, "stable")
	mgr := newCertManager(cert1, key1)
	if err := mgr.Load(cert1, key1); err != nil {
		t.Fatal(err)
	}

	// Corrupt the cert file (zero length is the cheapest realistic
	// "writer is mid-flight" state) and bump ModTime.
	if err := os.WriteFile(cert1, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(cert1, future, future); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := mgr.checkAndReload(logger)
	if err == nil {
		t.Fatal("expected an error reloading a corrupted cert file")
	}
	if got := mgr.leafFingerprint(); got != sha1 {
		t.Fatalf("corrupt-on-disk clobbered in-memory cert: got %s, want %s", got, sha1)
	}
}

// TestServerRunServesFreshlyLoadedCert is the integration regression
// test for A23: a real Server.Run serving over TLS must hand back the
// CURRENT cert from the cert manager — not a cert frozen at boot. We
// verify by hitting the listener with a TLS handshake and comparing
// the presented leaf against the cert-on-disk fingerprint.
func TestServerRunServesFreshlyLoadedCert(t *testing.T) {
	t.Parallel()

	cert1, key1, sha1 := writeSelfSignedCertKey(t, "served")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	upstream := newPingUpstream(t)
	defer upstream.Close()

	cfg := config.Default()
	cfg.Listen = addr
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}
	cfg.TLS = config.TLS{CertFile: cert1, KeyFile: key1}
	cfg.HTTP.CertReloadInterval = 0 // watcher off — boot-only load suffices

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov := simpleProviders()

	handler, err := New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, handler, logger)
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	runReturned := make(chan error, 1)
	go func() { runReturned <- srv.Run(runCtx) }()

	// Wait for listener.
	waitForListener(t, addr)

	// Open a raw TLS connection and capture the leaf.
	// InsecureSkipVerify is fine here: the test mints its own
	// self-signed cert and pins the SHA-256 of the leaf below — name
	// verification would only get in the way.
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	}
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	chain := conn.ConnectionState().PeerCertificates
	_ = conn.Close()
	if len(chain) == 0 {
		t.Fatal("no peer cert presented")
	}
	gotSum := sha256.Sum256(chain[0].Raw)
	if got := hex.EncodeToString(gotSum[:]); got != sha1 {
		t.Fatalf("served leaf fingerprint = %s, want %s — Server.Run is not pulling from certManager", got, sha1)
	}

	cancel()
	select {
	case <-runReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return after cancel")
	}
}

// pingUpstream is a tiny http.Server that always replies with a 200. We
// don't need any specific behaviour for the TLS-cert assertion — we
// only need the proxy to start cleanly.
type pingUpstream struct {
	URL    string
	server *http.Server
	ln     net.Listener
}

func (p *pingUpstream) Close() { _ = p.server.Close() }

func newPingUpstream(t *testing.T) *pingUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	return &pingUpstream{URL: "http://" + ln.Addr().String(), server: srv, ln: ln}
}

func simpleProviders() telemetry.Providers {
	tp := sdktrace.NewTracerProvider()
	meter := noop.NewMeterProvider().Meter("noop")
	tokens, _ := meter.Int64Histogram("gen_ai.client.token.usage")
	dur, _ := meter.Float64Histogram("gen_ai.client.operation.duration")
	ttft, _ := meter.Float64Histogram("gen_ai.client.time_to_first_token")
	cost, _ := meter.Float64Counter("gen_ai.client.cost.usd")
	return telemetry.Providers{
		Tracer: tp.Tracer("test"),
		Meter:  meter,
		Meters: telemetry.GenAIMeters{
			TokenUsage:        tokens,
			OperationDuration: dur,
			TimeToFirstToken:  ttft,
			CostUSD:           cost,
		},
		Shutdown: func(context.Context) error { return nil },
	}
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
