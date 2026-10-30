package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func runDemo(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8080", "mock upstream listen address")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/", demoResponse)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	_, _ = fmt.Fprintf(stderr, "llmtap demo upstream listening on http://%s\n", *listen)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func demoResponse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	if req.Model == "" {
		req.Model = "gpt-5-mini"
	}
	if req.Stream || strings.HasSuffix(r.URL.Path, "/responses") {
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/responses") {
			_, _ = io.WriteString(w, "event: response.output_text.delta\n")
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"Hello from llmtap demo."}`+"\n\n")
			_, _ = io.WriteString(w, "event: response.completed\n")
			_, _ = fmt.Fprintf(w, `data: {"type":"response.completed","response":{"id":"resp_demo","model":%q,"status":"completed","usage":{"input_tokens":8,"output_tokens":5}}}`+"\n\n", req.Model)
			return
		}
		_, _ = fmt.Fprintf(w, `data: {"id":"chatcmpl_demo","model":%q,"choices":[{"index":0,"delta":{"content":"Hello from llmtap demo."},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":5}}`+"\n\n", req.Model)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl_demo", "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "Hello from llmtap demo."}}},
		"usage": map[string]any{"prompt_tokens": 8, "completion_tokens": 5, "total_tokens": 13},
	})
}

func runGenCert(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gen-cert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "localhost", "DNS name or IP for the certificate")
	outDir := fs.String("out-dir", ".", "directory for tls.crt and tls.key")
	days := fs.Int("days", 365, "certificate validity in days")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *days < 1 {
		return errors.New("gen-cert: days must be positive")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("gen-cert: key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("gen-cert: serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: *host},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Duration(*days) * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(*host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{*host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("gen-cert: certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("gen-cert: marshal key: %w", err)
	}
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}
	certPath := filepath.Join(*outDir, "tls.crt")
	keyPath := filepath.Join(*outDir, "tls.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s and %s\n", certPath, keyPath)
	return nil
}
