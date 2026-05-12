package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestNewLoggerRespectsLevelInfo asserts that with level=info, Info
// records reach stderr but Debug records are filtered.
func TestNewLoggerRespectsLevelInfo(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	logger, err := newLogger("info", "llmtap", "test-version", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	logger.DebugContext(context.Background(), "debug-record-should-be-filtered")
	logger.InfoContext(context.Background(), "info-record-should-pass")

	out := stderr.String()
	if strings.Contains(out, "debug-record-should-be-filtered") {
		t.Errorf("debug record reached stderr at level=info: %q", out)
	}
	if !strings.Contains(out, "info-record-should-pass") {
		t.Errorf("info record missing from stderr at level=info: %q", out)
	}
}

// TestNewLoggerRespectsLevelError asserts that with level=error, both
// Info and Warn records are filtered while Error records pass.
func TestNewLoggerRespectsLevelError(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	logger, err := newLogger("error", "llmtap", "test-version", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	logger.InfoContext(context.Background(), "info-record-should-be-filtered")
	logger.WarnContext(context.Background(), "warn-record-should-be-filtered")
	logger.ErrorContext(context.Background(), "error-record-should-pass")

	out := stderr.String()
	if strings.Contains(out, "info-record-should-be-filtered") {
		t.Errorf("info record reached stderr at level=error: %q", out)
	}
	if strings.Contains(out, "warn-record-should-be-filtered") {
		t.Errorf("warn record reached stderr at level=error: %q", out)
	}
	if !strings.Contains(out, "error-record-should-pass") {
		t.Errorf("error record missing from stderr at level=error: %q", out)
	}
}

// TestNewLoggerInvalidLevel returns an error rather than silently
// degrading to a default.
func TestNewLoggerInvalidLevel(t *testing.T) {
	t.Parallel()
	if _, err := newLogger("verbose", "llmtap", "test", new(bytes.Buffer)); err == nil {
		t.Fatal("expected error on invalid level")
	}
}

// TestNewLoggerEmptyLevelDefaultsToInfo: the flag default is "info",
// but empty should also work the same way (mirrors the original
// switch's "info", "": clause).
func TestNewLoggerEmptyLevelDefaultsToInfo(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	logger, err := newLogger("", "llmtap", "test", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	logger.InfoContext(context.Background(), "info-passes")
	if !strings.Contains(stderr.String(), "info-passes") {
		t.Errorf("info record missing under empty-level default: %q", stderr.String())
	}
}

// TestNewLoggerAttachesServiceAttrs asserts the service.name and
// service.version attributes are present on every record.
func TestNewLoggerAttachesServiceAttrs(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	logger, err := newLogger("info", "llmtap-test-name", "v1.2.3", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	logger.InfoContext(context.Background(), "probe")
	out := stderr.String()
	if !strings.Contains(out, "service.name=llmtap-test-name") {
		t.Errorf("service.name missing: %q", out)
	}
	if !strings.Contains(out, "service.version=v1.2.3") {
		t.Errorf("service.version missing: %q", out)
	}
}

// silence the slog handler's default level when running individual
// tests with -run flags that don't include the bytes-buffer ones.
var _ = slog.LevelInfo
