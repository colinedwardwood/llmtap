// Package config loads and validates llmtap's runtime configuration.
//
// Precedence (lowest to highest): built-in defaults -> YAML file -> environment
// variables. Environment variables prefixed with LLMTAP_ override scalar
// fields (see EnvLookup). The full schema is documented in config.example.yaml.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the validated runtime config for the proxy.
type Config struct {
	Listen        string         `yaml:"listen"`
	AllowInsecure bool           `yaml:"allow_insecure"`
	TLS           TLS            `yaml:"tls"`
	Upstreams     []Upstream     `yaml:"upstreams"`
	Telemetry     Telemetry      `yaml:"telemetry"`
	Content       ContentCapture `yaml:"content"`
	HTTP          HTTPTimeouts   `yaml:"http"`
	Service       Service        `yaml:"service"`
	Request       Request        `yaml:"request"`
}

// Request shapes the proxy's handling of inbound request bodies.
type Request struct {
	// MaxBodyBytes is the hard ceiling above which the proxy refuses an
	// inbound request body with 413. Below this, bodies are forwarded
	// byte-for-byte even if they exceed the enrichment buffer used to
	// parse model / stream / token metadata. A value of 0 disables the
	// limit, which is rarely what the operator wants.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

// TLS configures listener-side TLS termination. Setting CertFile and KeyFile
// flips the listener to HTTPS. Setting ClientCAFile additionally requires
// every client to present a certificate signed by that CA — the right knob
// when llmtap is shared across services in a VPC and you want the proxy to
// be the audit boundary.
type TLS struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// Enabled reports whether listener-side TLS is configured.
func (t TLS) Enabled() bool { return t.CertFile != "" || t.KeyFile != "" }

// MTLS reports whether mutual TLS is configured.
func (t TLS) MTLS() bool { return t.ClientCAFile != "" }

// Upstream binds a path prefix to an upstream LLM API and a parser.
type Upstream struct {
	Name     string `yaml:"name"`
	Prefix   string `yaml:"prefix"`
	Target   string `yaml:"target"`
	Provider string `yaml:"provider"`
}

// Telemetry controls how llmtap exports its OTel signals.
type Telemetry struct {
	Endpoint    string        `yaml:"endpoint"`
	Protocol    string        `yaml:"protocol"`
	Insecure    bool          `yaml:"insecure"`
	Headers     Headers       `yaml:"headers"`
	SampleRatio float64       `yaml:"sample_ratio"`
	Timeout     time.Duration `yaml:"timeout"`
}

// Headers is a typed map so YAML round-trips and we never reach for `any`.
type Headers map[string]string

// ContentCapture controls capture of prompt/completion content. Defaults are
// privacy-preserving: capture metadata only, never content. See OTel GenAI
// semconv: GEN_AI_CAPTURE_MESSAGE_CONTENT.
type ContentCapture struct {
	// Mode is one of "off", "events", "logs". "off" emits only metadata
	// (model, tokens, finish reasons). "events" attaches content as span
	// events. "logs" emits content via the OTel log signal.
	Mode string `yaml:"mode"`
}

// HTTPTimeouts shapes the listening server's deadlines. The defaults are
// generous because LLM responses can stream for minutes.
type HTTPTimeouts struct {
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// Service identifies llmtap to the OTel backend (resource attributes).
type Service struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	Env       string `yaml:"environment"`
}

const (
	ProtoGRPC = "grpc"
	ProtoHTTP = "http"

	CaptureOff    = "off"
	CaptureEvents = "events"
	CaptureLogs   = "logs"

	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

// Default returns a config with sensible defaults for local development.
//
// The default listen address is loopback-only on purpose: the only way to
// expose llmtap on a non-loopback interface is to either configure TLS or
// explicitly set AllowInsecure (to acknowledge that you're carrying API
// credentials over plaintext on whatever network the listener faces).
func Default() Config {
	return Config{
		Listen: "127.0.0.1:4000",
		Upstreams: []Upstream{
			{Name: "openai", Prefix: "/v1", Target: "https://api.openai.com", Provider: ProviderOpenAI},
			{Name: "anthropic", Prefix: "/anthropic", Target: "https://api.anthropic.com", Provider: ProviderAnthropic},
		},
		Telemetry: Telemetry{
			Endpoint:    "localhost:4317",
			Protocol:    ProtoGRPC,
			Insecure:    true,
			SampleRatio: 1.0,
			Timeout:     10 * time.Second,
		},
		Content: ContentCapture{Mode: CaptureOff},
		HTTP: HTTPTimeouts{
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       120 * time.Second,
			ShutdownTimeout:   30 * time.Second,
		},
		Service: Service{
			Name:      "llmtap",
			Namespace: "observability",
			Env:       "dev",
		},
		Request: Request{
			// 32 MiB headroom for multimodal payloads (vision, audio).
			// Above this, llmtap returns 413 without contacting upstream.
			MaxBodyBytes: 32 * 1024 * 1024,
		},
	}
}

// Load merges defaults, an optional YAML file, and environment overrides.
// A zero-length path skips the YAML step (useful for tests and demos).
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // operator supplies the path
		if err != nil {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %q: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv mutates cfg in place from LLMTAP_* environment variables. Only
// scalar fields are overridable from env; structural changes (upstreams)
// belong in the YAML file or a separate config-management layer.
func applyEnv(cfg *Config) {
	if v, ok := os.LookupEnv("LLMTAP_LISTEN"); ok {
		cfg.Listen = v
	}
	if v, ok := os.LookupEnv("LLMTAP_ALLOW_INSECURE"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AllowInsecure = b
		}
	}
	if v, ok := os.LookupEnv("LLMTAP_TLS_CERT_FILE"); ok {
		cfg.TLS.CertFile = v
	}
	if v, ok := os.LookupEnv("LLMTAP_TLS_KEY_FILE"); ok {
		cfg.TLS.KeyFile = v
	}
	if v, ok := os.LookupEnv("LLMTAP_TLS_CLIENT_CA_FILE"); ok {
		cfg.TLS.ClientCAFile = v
	}
	if v, ok := os.LookupEnv("LLMTAP_OTLP_ENDPOINT"); ok {
		cfg.Telemetry.Endpoint = v
	}
	if v, ok := os.LookupEnv("LLMTAP_OTLP_PROTOCOL"); ok {
		cfg.Telemetry.Protocol = v
	}
	if v, ok := os.LookupEnv("LLMTAP_OTLP_INSECURE"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Telemetry.Insecure = b
		}
	}
	if v, ok := os.LookupEnv("LLMTAP_CAPTURE"); ok {
		cfg.Content.Mode = v
	}
	if v, ok := os.LookupEnv("LLMTAP_SERVICE_NAME"); ok {
		cfg.Service.Name = v
	}
	if v, ok := os.LookupEnv("LLMTAP_SERVICE_NAMESPACE"); ok {
		cfg.Service.Namespace = v
	}
	if v, ok := os.LookupEnv("LLMTAP_ENV"); ok {
		cfg.Service.Env = v
	}
}

// Validate reports the first violation it finds. The caller surfaces it to
// the user; we never start serving with an invalid config.
func (c *Config) Validate() error {
	var errs []error

	if strings.TrimSpace(c.Listen) == "" {
		errs = append(errs, errors.New("listen: must not be empty"))
	}

	// Listener-side TLS sanity. Two rules:
	//   1. If either cert_file or key_file is set, both must be.
	//   2. Refuse to listen on a non-loopback address without TLS unless the
	//      operator has explicitly opted in via allow_insecure.
	switch {
	case c.TLS.CertFile != "" && c.TLS.KeyFile == "":
		errs = append(errs, errors.New("tls.key_file: required when cert_file is set"))
	case c.TLS.KeyFile != "" && c.TLS.CertFile == "":
		errs = append(errs, errors.New("tls.cert_file: required when key_file is set"))
	}
	if c.TLS.MTLS() && !c.TLS.Enabled() {
		errs = append(errs, errors.New("tls.client_ca_file: requires tls.cert_file/key_file (mTLS implies TLS)"))
	}
	if !c.TLS.Enabled() && !c.AllowInsecure && !isLoopbackAddr(c.Listen) {
		errs = append(errs, fmt.Errorf(
			"listen %q is non-loopback but TLS is not configured: set tls.cert_file/key_file, or bind to 127.0.0.1, or set allow_insecure=true to acknowledge plaintext on this interface",
			c.Listen,
		))
	}

	if len(c.Upstreams) == 0 {
		errs = append(errs, errors.New("upstreams: at least one upstream required"))
	}
	seenPrefix := map[string]string{}
	for i, u := range c.Upstreams {
		if u.Name == "" {
			errs = append(errs, fmt.Errorf("upstreams[%d].name: required", i))
		}
		if !strings.HasPrefix(u.Prefix, "/") {
			errs = append(errs, fmt.Errorf("upstreams[%d].prefix: must start with '/'", i))
		}
		if other, dup := seenPrefix[u.Prefix]; dup {
			errs = append(errs, fmt.Errorf("upstreams[%d].prefix %q: duplicate of upstream %q", i, u.Prefix, other))
		} else {
			seenPrefix[u.Prefix] = u.Name
		}
		if _, err := url.Parse(u.Target); err != nil || u.Target == "" {
			errs = append(errs, fmt.Errorf("upstreams[%d].target %q: invalid URL", i, u.Target))
		}
		switch u.Provider {
		case ProviderOpenAI, ProviderAnthropic:
		default:
			errs = append(errs, fmt.Errorf("upstreams[%d].provider %q: unsupported (want openai|anthropic)", i, u.Provider))
		}
	}

	switch c.Telemetry.Protocol {
	case ProtoGRPC, ProtoHTTP:
	default:
		errs = append(errs, fmt.Errorf("telemetry.protocol %q: must be grpc|http", c.Telemetry.Protocol))
	}
	if c.Telemetry.SampleRatio < 0 || c.Telemetry.SampleRatio > 1 {
		errs = append(errs, fmt.Errorf("telemetry.sample_ratio %v: must be in [0,1]", c.Telemetry.SampleRatio))
	}

	switch c.Content.Mode {
	case CaptureOff, CaptureEvents, CaptureLogs, "":
		if c.Content.Mode == "" {
			c.Content.Mode = CaptureOff
		}
	default:
		errs = append(errs, fmt.Errorf("content.mode %q: must be off|events|logs", c.Content.Mode))
	}

	return errors.Join(errs...)
}

// isLoopbackAddr reports whether the configured listen address binds to a
// loopback interface only. The bare ":port" form binds to all interfaces and
// is treated as non-loopback. Hostnames other than "localhost" are also
// treated as non-loopback because we can't resolve them safely at config
// time without surprising DNS dependencies.
func isLoopbackAddr(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// Match returns the upstream whose Prefix is the longest match of urlPath, or
// (Upstream{}, false) if none. Longest-prefix-match keeps routing predictable
// when prefixes nest (e.g. "/v1" and "/v1/embeddings").
func (c *Config) Match(urlPath string) (Upstream, bool) {
	var best Upstream
	bestLen := -1
	for _, u := range c.Upstreams {
		if !strings.HasPrefix(urlPath, u.Prefix) {
			continue
		}
		if len(u.Prefix) > bestLen {
			best = u
			bestLen = len(u.Prefix)
		}
	}
	return best, bestLen >= 0
}
