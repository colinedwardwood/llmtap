package proxy_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
)

// generateTestCert returns a self-signed cert with the given SAN and
// the hex-encoded SPKI sha256 pin for the leaf. The cert is valid for
// 1 hour — plenty for a test run, short enough that an accidental
// commit of a never-rotated identity is obvious.
func generateTestCert(t *testing.T, san string) (*tls.Certificate, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: san},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{san, "localhost"},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := x509.ParseCertificate(derBytes)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	return &cert, hex.EncodeToString(sum[:])
}

// newTLSUpstream returns an httptest.Server using the supplied cert
// and a handler that records every request. The caller is responsible
// for closing the server.
func newTLSUpstream(t *testing.T, cert *tls.Certificate) (*httptest.Server, *bool) {
	t.Helper()
	hit := false
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[],"usage":{}}`)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	return srv, &hit
}

// rebuildClientToTrust returns an http.Client that trusts the supplied
// upstream's TLS roots. Used to verify pinning behaviour at the proxy
// layer rather than at the client layer.
func rebuildClientToTrust(upstream *httptest.Server) *http.Client {
	return upstream.Client()
}

// TestUpstreamServerNameAlwaysSet asserts the per-upstream Transport
// pins ServerName to the parsed target's hostname so SNI is
// deterministic and a swapped DNS entry can't quietly downgrade
// certificate validation.
func TestUpstreamServerNameAlwaysSet(t *testing.T) {
	t.Parallel()

	cert, _ := generateTestCert(t, "llmtap-upstream-test")
	upstream, _ := newTLSUpstream(t, cert)
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
	}}

	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rps := proxy.ReverseProxiesForTest(h)
	rp, ok := rps["openai"]
	if !ok {
		t.Fatal("upstream missing from rps map")
	}
	tr, ok := rp.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is not *http.Transport: %T", rp.Transport)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}
	if tr.TLSClientConfig.ServerName != target.Hostname() {
		t.Errorf("TLSClientConfig.ServerName = %q; want %q", tr.TLSClientConfig.ServerName, target.Hostname())
	}
}

// TestUpstreamPinAcceptsMatchingCert verifies that a request flows
// when the upstream's leaf SPKI matches the configured pin.
func TestUpstreamPinAcceptsMatchingCert(t *testing.T) {
	t.Parallel()

	cert, pin := generateTestCert(t, "llmtap-upstream-test")
	upstream, hit := newTLSUpstream(t, cert)
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		PinSHA256: []string{pin},
	}}

	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Trust the test cert at the OUTBOUND transport too — the pin
	// runs AFTER stdlib chain verification, so without trusting the
	// CA the request fails at the chain check before reaching the
	// pin. In production the upstream's cert is rooted in the system
	// trust store and the pin is the extra check; this test isolates
	// the pin layer.
	pool := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(leaf)
	rps := proxy.ReverseProxiesForTest(h)
	tr := rps["openai"].Transport.(*http.Transport)
	tr.TLSClientConfig.RootCAs = pool

	ts := httptest.NewServer(h)
	defer ts.Close()

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 (pin matches, request should succeed)", resp.StatusCode)
	}
	if !*hit {
		t.Fatal("upstream was never hit")
	}
}

// TestUpstreamPinRejectsWrongCert verifies that a non-matching pin
// stops the connection at the proxy → upstream TLS handshake.
func TestUpstreamPinRejectsWrongCert(t *testing.T) {
	t.Parallel()

	cert, _ := generateTestCert(t, "llmtap-upstream-test")
	upstream, hit := newTLSUpstream(t, cert)
	defer upstream.Close()

	// A non-matching pin: 64 zero hex chars never matches any real
	// SPKI digest.
	wrongPin := strings.Repeat("0", 64)

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		PinSHA256: []string{wrongPin},
	}}
	h, err := proxy.New(cfg, provider.BuiltIn(), fakeProv(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Trust the chain so the failure is unambiguously from the pin
	// check rather than the unknown-CA error stdlib raises first.
	pool := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(leaf)
	rps := proxy.ReverseProxiesForTest(h)
	tr := rps["openai"].Transport.(*http.Transport)
	tr.TLSClientConfig.RootCAs = pool

	ts := httptest.NewServer(h)
	defer ts.Close()

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// httputil's ReverseProxy maps transport-layer errors to 502.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (pin mismatch should fail handshake → 502)", resp.StatusCode)
	}
	if *hit {
		t.Error("upstream was hit despite pin mismatch — verify happened too late")
	}
}

// Sanity check helper to keep the build clean: catch the explicit
// errors helpful for diagnosing flakes.
var _ = errors.New
var _ = rebuildClientToTrust
