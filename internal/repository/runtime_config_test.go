package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRuntimeConfigGeneratesReceiverToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg, err := LoadRuntimeConfig(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Receiver.Token) != 64 {
		t.Fatalf("expected generated 64-char receiver token, got %q", cfg.Receiver.Token)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `token = "`) {
		t.Fatalf("expected generated token to be written to config: %s", string(data))
	}
}

func TestLoadRuntimeConfigFillsEmptyReceiverToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[receiver]\ntoken = \"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRuntimeConfig(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Receiver.Token) != 64 {
		t.Fatalf("expected generated 64-char receiver token, got %q", cfg.Receiver.Token)
	}
}
