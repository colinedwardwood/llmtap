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
	server          *http.Server
	handler         *Handler
	listen          string
	timeout         time.Duration
	logger          *slog.Logger
	certFile        string
	keyFile         string
	certMgr         *certManager
	certReloadEvery time.Duration
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

	var certMgr *certManager
	if cfg.TLS.Enabled() {
		certMgr = newCertManager(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err := certMgr.Load(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		tlsCfg, err := buildTLSConfig(cfg.TLS, certMgr)
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		srv.TLSConfig = tlsCfg
		// Seed the NotAfter gauge and warn if the boot cert is close
		// to expiry — silent age-into-outage was MANDATORY M11.
		h.observeCert(certMgr.leafNotAfter(), logger)
	}

	return &Server{
		server:          srv,
		handler:         h,
		listen:          cfg.Listen,
		timeout:         cfg.HTTP.ShutdownTimeout,
		logger:          logger,
		certFile:        cfg.TLS.CertFile,
		keyFile:         cfg.TLS.KeyFile,
		certMgr:         certMgr,
		certReloadEvery: cfg.HTTP.CertReloadInterval,
	}, nil
}

// buildTLSConfig produces a TLS 1.2+ config. When ClientCAFile is set, every
// connecting client must present a certificate chained to that CA — turning
// llmtap into a hard policy boundary instead of an ambient one.
//
// When mgr is non-nil, GetCertificate is wired so the listener resolves
// certs per-handshake from the manager's atomic cache. That lets a watcher
// goroutine swap renewed certs in without dropping the listener.
func buildTLSConfig(t config.TLS, mgr *certManager) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if mgr != nil {
		cfg.GetCertificate = mgr.Get
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

	// Start the cert reload watcher BEFORE Serve so a renewal that lands
	// in the first 30s after boot still picks up. The watcher exits when
	// ctx is cancelled, so it lives exactly as long as Run does.
	if s.certMgr != nil && s.certReloadEvery > 0 {
		go s.certMgr.Watch(ctx, s.certReloadEvery, s.logger, s.handler)
	}

	errCh := make(chan error, 1)
	go func() {
		var serveErr error
		if s.tlsEnabled() {
			// Empty paths route the cert load through the configured
			// GetCertificate on TLSConfig — i.e. through the cert
			// manager — instead of LoadX509KeyPair at boot. That's
			// what makes hot-reload work: ServeTLS never re-reads the
			// file paths after this single call.
			serveErr = s.server.ServeTLS(ln, "", "")
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
		// Flip /readyz to 503 immediately so the load balancer
		// stops sending new traffic before we start draining
		// (k8s: pair with readinessProbe + preStop / grace period).
		if s.handler.beginDrain != nil {
			s.handler.beginDrain()
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		// Shutdown FIRST (concurrently): it closes the listener so no
		// new work arrives, then waits for in-flight handlers. The old
		// order — WaitForStreams before Shutdown — drained streams
		// while the listener was still accepting, so under sustained
		// traffic activeStreams never reached zero, the entire budget
		// burned, and Shutdown then ran with an expired context: a
		// hard kill for everything, including short requests that
		// would have drained in milliseconds.
		shutdownErr := make(chan error, 1)
		go func() { shutdownErr <- s.server.Shutdown(shutCtx) }()
		// Drain active SSE streams in parallel on the same budget.
		// Shutdown waits for ServeHTTP to return, but the SSE wrappers
		// run their finalize from a body-close callback that races
		// independently of the response-handler goroutine. Polling
		// activeStreams gives that finalize a chance to record its
		// metrics / end its span before the connection sweep.
		if remaining := s.handler.WaitForStreams(shutCtx); remaining > 0 {
			s.logger.WarnContext(shutCtx, "shutdown timeout reached with streams still in flight",
				slog.Int64("remaining", remaining),
			)
		}
		if err := <-shutdownErr; err != nil {
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
