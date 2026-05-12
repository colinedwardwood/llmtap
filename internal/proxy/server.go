package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/colinedwardwood/llmtap/internal/config"
)

// Server wraps a net/http.Server and a single Handler. It owns startup,
// graceful shutdown, and the listener — nothing else.
type Server struct {
	server   *http.Server
	listen   string
	timeout  time.Duration
	logger   *slog.Logger
	certFile string
	keyFile  string
}

// NewServer wires a Handler into an http.Server with conservative timeouts
// (read-header bounded; idle bounded; no overall write timeout because LLM
// streams legitimately last minutes). When cfg.TLS is configured, the
// server's TLSConfig is built here so mTLS (if requested via ClientCAFile)
// is in place before ServeTLS swaps the listener.
func NewServer(cfg config.Config, h *Handler, logger *slog.Logger) (*Server, error) {
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h.WrapWithOTel("llmtap"),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		// ReadTimeout bounds the inbound request, headers + body. Slow
		// uploaders trying to pin a goroutine for the full body
		// duration trip it. WriteTimeout stays zero so streaming
		// responses (which can legitimately run for minutes) aren't
		// killed by a server-side deadline; per-stream timeouts belong
		// on the client.
		ReadTimeout: cfg.HTTP.BodyReadTimeout,
		IdleTimeout: cfg.HTTP.IdleTimeout,
	}

	if cfg.TLS.Enabled() {
		tlsCfg, err := buildTLSConfig(cfg.TLS)
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		srv.TLSConfig = tlsCfg
	}

	return &Server{
		server:   srv,
		listen:   cfg.Listen,
		timeout:  cfg.HTTP.ShutdownTimeout,
		logger:   logger,
		certFile: cfg.TLS.CertFile,
		keyFile:  cfg.TLS.KeyFile,
	}, nil
}

// buildTLSConfig produces a TLS 1.2+ config. When ClientCAFile is set, every
// connecting client must present a certificate chained to that CA — turning
// llmtap into a hard policy boundary instead of an ambient one.
func buildTLSConfig(t config.TLS) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if t.ClientCAFile != "" {
		pem, err := os.ReadFile(t.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read client CA %q: %w", t.ClientCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("client CA %q: no PEM certificates parsed", t.ClientCAFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// Run blocks until ctx is cancelled or the server fails to start. On ctx
// cancellation it triggers a graceful shutdown bounded by ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.listen, err)
	}
	scheme := "http"
	if s.tlsEnabled() {
		scheme = "https"
	}
	s.logger.InfoContext(ctx, "listening",
		slog.String("addr", ln.Addr().String()),
		slog.String("scheme", scheme),
		slog.Bool("mtls", s.server.TLSConfig != nil && s.server.TLSConfig.ClientAuth >= tls.RequireAndVerifyClientCert),
	)

	errCh := make(chan error, 1)
	go func() {
		var serveErr error
		if s.tlsEnabled() {
			serveErr = s.server.ServeTLS(ln, s.certFile, s.keyFile)
		} else {
			serveErr = s.server.Serve(ln)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.InfoContext(ctx, "shutdown requested")
		shutCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		if err := s.server.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) tlsEnabled() bool {
	return s.certFile != "" && s.keyFile != ""
}
