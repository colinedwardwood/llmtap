package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/auth"
	"github.com/colinedwardwood/llmtap/internal/config"
)

func TestCallerForRequestPriority(t *testing.T) {
	t.Parallel()
	h := &Handler{cfg: config.Config{Attribution: config.Attribution{Header: "X-Caller"}}}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Caller", "team-search")
	req.TLS = &tls.ConnectionState{}
	req.TLS.PeerCertificates = []*x509.Certificate{{Subject: pkix.Name{CommonName: "service-a"}}}
	if got := h.callerForRequest(req); got != "team-search" {
		t.Fatalf("caller = %q", got)
	}
	if got := req.Header.Get("X-Caller"); got != "" {
		t.Fatalf("attribution header forwarded: %q", got)
	}
}

func TestCallerForRequestMTLS(t *testing.T) {
	t.Parallel()
	h := &Handler{cfg: config.Config{}}
	req := httptest.NewRequest("POST", "/", nil)
	req.TLS = &tls.ConnectionState{}
	req.TLS.PeerCertificates = []*x509.Certificate{{Subject: pkix.Name{CommonName: "service-a"}}}
	if got := h.callerForRequest(req); got != "mtls:service-a" {
		t.Fatalf("caller = %q", got)
	}
}

func TestCallerForRequestAuthFingerprint(t *testing.T) {
	t.Parallel()
	hash := "$argon2id$v=19$m=1,t=1,p=1$YQ$YQ"
	h := &Handler{
		cfg:        config.Config{},
		auth:       mustVerifierForAttributionTest(t, hash),
		authHeader: "X-LLMTAP-Token",
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-LLMTAP-Token", "Bearer secret-token")
	got := h.callerForRequest(req)
	if !strings.HasPrefix(got, "token:") || strings.Contains(got, "secret") {
		t.Fatalf("unsafe fingerprint = %q", got)
	}
}

func mustVerifierForAttributionTest(t *testing.T, hash string) *auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier([]string{hash})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
