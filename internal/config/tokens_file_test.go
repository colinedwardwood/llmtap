package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTokensFile(t *testing.T) {
	t.Parallel()
	raw := []byte(`
# comment
$argon2id$v=19$m=65536,t=3,p=2$YWFhYWFhYWFhYWFhYWFhYQ$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA

$argon2id$v=19$m=65536,t=3,p=2$YmJiYmJiYmJiYmJiYmJiYg$BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB
`)
	got, err := parseTokensFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestParseTokensFileRejectsPlaintext(t *testing.T) {
	t.Parallel()
	_, err := parseTokensFile([]byte("not-a-hash\n"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadTokensFileMerged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	hash := "$argon2id$v=19$m=65536,t=3,p=2$YWFhYWFhYWFhYWFhYWFhYQ$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := os.WriteFile(path, []byte(hash+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "cfg.yaml")
	yaml := `
listen: 127.0.0.1:4000
telemetry:
  endpoint: localhost:4317
  insecure: true
upstreams:
  - name: openai
    prefix: /v1
    target: https://api.openai.com
    provider: openai
auth:
  tokens_file: ` + path + `
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.Enabled() || len(cfg.Auth.Tokens) != 1 {
		t.Fatalf("tokens = %#v", cfg.Auth.Tokens)
	}
}
