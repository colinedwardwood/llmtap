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
	"syscall"

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
	case "version", "--version", "-v":
		_, _ = fmt.Fprintf(stderr, "llmtap %s (commit %s, built %s)\n",
			buildinfo.Version, buildinfo.Commit, buildinfo.Date)
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
  version     print version, commit, and build date

Run "llmtap up --help" for the up flags.`)
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

	// otelslog bridges slog → OTel logs once telemetry is up; until then we
	// log to stderr so misconfigurations are visible.
	logger := slog.New(otelslog.NewHandler("llmtap",
		otelslog.WithSource(true),
	)).With(
		slog.String("service.name", cfg.Service.Name),
		slog.String("service.version", buildinfo.Version),
	)
	if err := setLevel(*logLevel); err != nil {
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

// setLevel mutates the default slog level. Kept tiny: the otelslog handler
// honours LevelVar via the standard slog plumbing, but the bridge does not
// expose a setter directly, so we use the global default level.
func setLevel(level string) error {
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
		return fmt.Errorf("invalid log level %q", level)
	}
	// stdlib slog's default level is governed by handlers; for otelslog the
	// bridge respects level on emit. Calling SetLogLoggerLevel keeps the
	// stdlib log package consistent for any indirect consumers.
	slog.SetLogLoggerLevel(lvl)
	return nil
}
