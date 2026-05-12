package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
)

func TestBuildTLSConfigNoClientCA(t *testing.T) {
	t.Parallel()
	cfg, err := buildTLSConfig(config.TLS{CertFile: "x", KeyFile: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v want NoClientCert", cfg.ClientAuth)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want >= TLS 1.2", cfg.MinVersion)
	}
}

func TestBuildTLSConfigWithClientCA(t *testing.T) {
	t.Parallel()
	caPath := writeSelfSignedCAPEM(t)

	cfg, err := buildTLSConfig(config.TLS{
		CertFile:     "x",
		KeyFile:      "y",
		ClientCAFile: caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Fatal("ClientCAs pool not built")
	}
}

func TestBuildTLSConfigInvalidClientCA(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(bad, []byte("not a pem file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildTLSConfig(config.TLS{
		CertFile:     "x",
		KeyFile:      "y",
		ClientCAFile: bad,
	}); err == nil {
		t.Fatal("expected error for non-PEM client CA file")
	}

	if _, err := buildTLSConfig(config.TLS{
		CertFile:     "x",
		KeyFile:      "y",
		ClientCAFile: filepath.Join(dir, "missing.pem"),
	}); err == nil {
		t.Fatal("expected error for missing client CA file")
	}
}

// writeSelfSignedCAPEM writes a freshly minted self-signed CA cert to a temp
// file and returns its path. Deterministic enough for test, fast enough not
// to slow the suite (P-256 keygen on modern hardware ~ms).
func writeSelfSignedCAPEM(t *testing.T) string {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "llmtap-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	return path
}
