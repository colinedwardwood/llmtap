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
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/colinedwardwood/llmtap/internal/auth"
	"github.com/colinedwardwood/llmtap/internal/buildinfo"
	"github.com/colinedwardwood/llmtap/internal/config"
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
	case "hash-token":
		return runHashToken(rest, os.Stdin, os.Stdout, stderr)
	case "version", "--version", "-v":
		version, commit, date := buildinfo.Resolve()
		_, _ = fmt.Fprintf(stderr, "llmtap %s (commit %s, built %s)\n",
			version, commit, date)
		return nil
	case "help", "--help", "-h":
		usage(stderr)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `llmtap — OpenTelemetry tap for any LLM API

Commands:
  up          run the proxy
  hash-token  read a bearer token from stdin, print the argon2id hash
              suitable for auth.tokens in config.yaml
  version     print version, commit, and build date

Run "llmtap up --help" for the up flags.`)
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

	// Set up the root context now so signal-driven shutdown propagates to
	// the OTel batch exporters and the HTTP server alike.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prov, err := telemetry.Setup(ctx, cfg)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	version, _, _ := buildinfo.Resolve()
	logger, err := newLogger(*logLevel, cfg.Service.Name, version, stderr)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

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
