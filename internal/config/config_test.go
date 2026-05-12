package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestLoadYAMLOverrides(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := []byte(`
listen: "127.0.0.1:9999"
upstreams:
  - name: openai
    prefix: /v1
    target: https://api.openai.com
    provider: openai
telemetry:
  endpoint: tempo:4317
  protocol: grpc
  sample_ratio: 0.5
content:
  mode: events
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.Telemetry.SampleRatio != 0.5 {
		t.Errorf("sample_ratio = %v", cfg.Telemetry.SampleRatio)
	}
	if cfg.Content.Mode != CaptureEvents {
		t.Errorf("content.mode = %q", cfg.Content.Mode)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("LLMTAP_LISTEN", "127.0.0.1:12345")
	t.Setenv("LLMTAP_OTLP_ENDPOINT", "tempo.svc:4317")
	t.Setenv("LLMTAP_CAPTURE", "events")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:12345" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.Telemetry.Endpoint != "tempo.svc:4317" {
		t.Errorf("endpoint = %q", cfg.Telemetry.Endpoint)
	}
	if cfg.Content.Mode != CaptureEvents {
		t.Errorf("capture = %q", cfg.Content.Mode)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"empty listen", func(c *Config) { c.Listen = "" }},
		{"no upstreams", func(c *Config) { c.Upstreams = nil }},
		{"bad prefix", func(c *Config) { c.Upstreams[0].Prefix = "v1" }},
		{"bad provider", func(c *Config) { c.Upstreams[0].Provider = "cohere" }},
		{"bad protocol", func(c *Config) { c.Telemetry.Protocol = "tcp" }},
		{"bad ratio", func(c *Config) { c.Telemetry.SampleRatio = 2 }},
		{"bad capture", func(c *Config) { c.Content.Mode = "verbose" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mut(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestNonLoopbackRequiresTLSOrOptIn(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Listen = "0.0.0.0:4000"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-loopback without TLS or allow_insecure must be rejected")
	}

	cfg.AllowInsecure = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("allow_insecure should permit non-loopback plaintext: %v", err)
	}

	cfg.AllowInsecure = false
	cfg.TLS = TLS{CertFile: "/tmp/c.pem", KeyFile: "/tmp/k.pem"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("TLS should permit non-loopback: %v", err)
	}
}

func TestTLSPartialIsRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		tls  TLS
	}{
		{"only cert", TLS{CertFile: "/tmp/c.pem"}},
		{"only key", TLS{KeyFile: "/tmp/k.pem"}},
		{"client CA without TLS", TLS{ClientCAFile: "/tmp/ca.pem"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.TLS = tc.tls
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:4000", true},
		{"localhost:4000", true},
		{"LOCALHOST:4000", true},
		{"[::1]:4000", true},
		{":4000", false},
		{"0.0.0.0:4000", false},
		{"10.0.0.5:4000", false},
		{"example.com:4000", false},
		{"not-a-host", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.listen); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v want %v", tc.listen, got, tc.want)
		}
	}
}

func TestMatchLongestPrefix(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Upstreams = append(cfg.Upstreams, Upstream{
		Name: "openai-emb", Prefix: "/v1/embeddings",
		Target: "https://api.openai.com", Provider: ProviderOpenAI,
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	got, ok := cfg.Match("/v1/embeddings")
	if !ok || got.Name != "openai-emb" {
		t.Fatalf("longest-prefix match failed: %+v ok=%v", got, ok)
	}
	got, ok = cfg.Match("/v1/chat/completions")
	if !ok || got.Name != "openai" {
		t.Fatalf("default openai prefix failed: %+v ok=%v", got, ok)
	}
	if _, ok := cfg.Match("/random"); ok {
		t.Fatal("expected no match for /random")
	}
}
