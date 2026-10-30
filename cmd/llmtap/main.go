// Command llmtap runs a transparent reverse proxy in front of LLM provider
// APIs and emits OpenTelemetry traces, metrics, and logs that follow the
// GenAI semantic conventions.
//
// Usage:
//
//	llmtap up [--config FILE]
//	llmtap version
//
// All scalar config can be overridden via LLMTAP_* environment variables;
// see the README and config.example.yaml for the full schema.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/colinedwardwood/llmtap/internal/auth"
	"github.com/colinedwardwood/llmtap/internal/buildinfo"
	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/pricing"
	"github.com/colinedwardwood/llmtap/internal/provider"
	"github.com/colinedwardwood/llmtap/internal/proxy"
	"github.com/colinedwardwood/llmtap/internal/telemetry"

	"go.opentelemetry.io/contrib/bridges/otelslog"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "llmtap:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command provided")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "up":
		return runUp(rest, stderr)
	case "check":
		return runCheck(rest, os.Stdout, stderr)
	case "healthcheck":
		return runHealthcheck(rest, stderr)
	case "demo":
		return runDemo(rest, stderr)
	case "gen-cert":
		return runGenCert(rest, os.Stdout, stderr)
	case "replay":
		return runReplay(rest, os.Stdout, stderr)
	case "diff":
		return runDiff(rest, os.Stdout, stderr)
	case "hash-token":
		return runHashToken(rest, os.Stdin, os.Stdout, stderr)
	case "version", "--version", "-v":
		version, commit, date := buildinfo.Resolve()
		// stdout, not stderr: `llmtap version | grep …` and CI scripts
		// consume this.
		_, _ = fmt.Fprintf(os.Stdout, "llmtap %s (commit %s, built %s, pricing snapshot %s)\n",
			version, commit, date, pricing.Snapshot)
		return nil
	case "help", "--help", "-h":
		usage(os.Stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `llmtap — OpenTelemetry tap for OpenAI- and Anthropic-compatible LLM APIs

Commands:
  up          run the proxy
  check       load + validate config (defaults, YAML, env) and exit;
              exit code 0 means "llmtap up" would start with it
  healthcheck GET /healthz (for Docker HEALTHCHECK / k8s exec probes
              when no curl is in the image)
  demo        run a local OpenAI-compatible mock upstream (no API key)
  gen-cert    generate a local ECDSA certificate and key
  replay      replay a redacted JSONL recording against an upstream
  diff        compare two JSONL recording files
  hash-token  read a bearer token from stdin, print the argon2id hash
              suitable for auth.tokens in config.yaml
  version     print version, commit, and build date

Run "llmtap up --help" for the up flags.`)
}

// runCheck runs the exact Load+Validate path `up` uses (defaults →
// YAML → env → Validate) and reports the outcome without binding a
// listener or dialing the collector. CI pipelines and pre-deploy hooks
// get the config verdict for exit-code money.
func runCheck(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to YAML config (optional; env vars and defaults still apply)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	src := *configPath
	if src == "" {
		src = "(defaults + env only)"
	}
	_, _ = fmt.Fprintf(stdout, "config OK: %s\n", src)
	_, _ = fmt.Fprintf(stdout, "  listen: %s (tls=%v mtls=%v auth=%v)\n",
		cfg.Listen, cfg.TLS.Enabled(), cfg.TLS.MTLS(), cfg.Auth.Enabled())
	for _, u := range cfg.Upstreams {
		_, _ = fmt.Fprintf(stdout, "  upstream %s: %s -> %s (provider=%s strip_prefix=%v)\n",
			u.Name, u.Prefix, u.Target, u.Provider, u.StripPrefix)
	}
	_, _ = fmt.Fprintf(stdout, "  telemetry: %s (%s, insecure=%v, sample=%v)\n",
		cfg.Telemetry.Endpoint, cfg.Telemetry.Protocol, cfg.Telemetry.Insecure, cfg.Telemetry.SampleRatio)
	_, _ = fmt.Fprintf(stdout, "  content: mode=%s redact=%s\n", cfg.Content.Mode, cfg.Content.Redact)
	return nil
}

// runHealthcheck hits the process's /healthz. Used as the container
// HEALTHCHECK and as a kubelet exec probe substitute in distroless
// images (no curl/wget). Exit 0 only on HTTP 200.
//
// Address resolution: --url flag, else LLMTAP_HEALTHCHECK_URL, else
// http://127.0.0.1:4000/healthz (matches Default().Listen).
func runHealthcheck(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "full healthz URL (default from LLMTAP_HEALTHCHECK_URL or http://127.0.0.1:4000/healthz)")
	timeout := fs.Duration("timeout", 2*time.Second, "HTTP client timeout")
	insecure := fs.Bool("insecure", false, "skip TLS verify (localhost HTTPS with self-signed certs)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	u := *urlFlag
	if u == "" {
		u = os.Getenv("LLMTAP_HEALTHCHECK_URL")
	}
	if u == "" {
		u = "http://127.0.0.1:4000/healthz"
	}
	if v := os.Getenv("LLMTAP_HEALTHCHECK_INSECURE"); v == "1" || strings.EqualFold(v, "true") {
		*insecure = true
	}
	if err := validateHealthcheckURL(u); err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	client := &http.Client{Timeout: *timeout}
	if *insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local probe only
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	// URL host is restricted to loopback by validateHealthcheckURL above.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil) //nolint:gosec // G704: loopback-only
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	resp, err := client.Do(req) //nolint:gosec // G704: loopback-only
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned %d", u, resp.StatusCode)
	}
	return nil
}

// validateHealthcheckURL confines the probe to loopback so a hostile
// LLMTAP_HEALTHCHECK_URL cannot turn the binary into an SSRF trampoline
// inside the container network namespace.
func validateHealthcheckURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("url scheme must be http or https")
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("url missing host")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("url host %q is not loopback (healthcheck only probes this process)", host)
	}
	return nil
}

// runHashToken reads a plaintext bearer token from stdin and writes the
// argon2id PHC-encoded hash to stdout. Reading from stdin avoids putting
// the secret in the shell history or argv (visible via /proc).
func runHashToken(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hash-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	plain := strings.TrimRight(string(raw), "\r\n")
	if plain == "" {
		return errors.New("hash-token: empty token on stdin")
	}
	encoded, err := auth.Hash(plain)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, encoded)
	return nil
}

func runUp(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to YAML config (optional; env vars and defaults still apply)")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	version, _, _ := buildinfo.Resolve()
	logger, err := newLogger(*logLevel, cfg.Service.Name, version, stderr)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	// Set up the root context now so signal-driven shutdown propagates to
	// the OTel batch exporters and the HTTP server alike.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prov, err := telemetry.Setup(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	logger.InfoContext(ctx, "llmtap starting",
		slog.String("listen", cfg.Listen),
		slog.String("otlp.endpoint", cfg.Telemetry.Endpoint),
		slog.String("otlp.protocol", cfg.Telemetry.Protocol),
		slog.String("content.mode", cfg.Content.Mode),
		slog.Int("upstreams", len(cfg.Upstreams)),
	)

	handler, err := proxy.New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	srv, err := proxy.NewServer(cfg, handler, logger)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	runErr := srv.Run(ctx)

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := prov.Shutdown(shutCtx); err != nil {
		logger.ErrorContext(shutCtx, "telemetry shutdown", slog.Any("err", err))
	}
	logger.InfoContext(ctx, "llmtap stopped")
	return runErr
}

// newLogger constructs the process logger. It fans records out to two
// sinks via a multiHandler:
//
//   - A leveled slog.TextHandler on stderr, so operators see logs in
//     the terminal subject to the --log-level filter.
//   - The otelslog bridge, wrapped with a leveling filter, so OTLP
//     export honours the same level.
//
// Both sinks share a single slog.LevelVar so changing the level changes
// both paths in lockstep. The otelslog handler has no native level
// option in the pinned SDK version, hence the explicit leveledHandler
// wrap — a no-op setter like slog.SetLogLoggerLevel does NOT filter the
// bridge.
func newLogger(level, serviceName, serviceVersion string, stderr io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(lvl)

	textHandler := slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level:     levelVar,
		AddSource: true,
	})
	otelHandler := &leveledHandler{
		next: otelslog.NewHandler("llmtap", otelslog.WithSource(true)),
		lvl:  levelVar,
	}

	multi := multiHandler{textHandler, otelHandler}
	return slog.New(multi).With(
		slog.String("service.name", serviceName),
		slog.String("service.version", serviceVersion),
	), nil
}

// leveledHandler wraps a downstream slog.Handler with a level filter.
// Used to give the otelslog bridge a level-aware shape since the bridge
// itself emits every record it sees.
type leveledHandler struct {
	next slog.Handler
	lvl  *slog.LevelVar
}

func (h *leveledHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.lvl.Level()
}

func (h *leveledHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.next.Handle(ctx, r)
}

func (h *leveledHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &leveledHandler{next: h.next.WithAttrs(attrs), lvl: h.lvl}
}

func (h *leveledHandler) WithGroup(name string) slog.Handler {
	return &leveledHandler{next: h.next.WithGroup(name), lvl: h.lvl}
}

// multiHandler dispatches each record to every underlying handler whose
// Enabled returns true for the record's level. Errors from individual
// handlers are joined so a misbehaving sink can't drop the rest.
type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}
