package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAcceptsDocumentedLoopbackYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	content := `services:
  reports:
    target: http://127.0.0.1:8000
    description: "Reporting API"
  dashboard:
    target: http://localhost:3000
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if got := len(cfg.Services); got != 2 {
		t.Fatalf("len(services) = %d, want 2", got)
	}
	if got := cfg.Services["reports"].Target; got != "http://127.0.0.1:8000" {
		t.Fatalf("reports target = %q, want http://127.0.0.1:8000", got)
	}
	if got := cfg.Services["dashboard"].Description; got != "" {
		t.Fatalf("dashboard description = %q, want empty", got)
	}
}

func TestLoadRejectsNonLoopbackTargets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	content := `services:
  reports:
    target: http://192.168.1.50:8000
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Load() error = %v, want loopback validation message", err)
	}
}

func TestLoadRejectsUnsafeServiceNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	content := `services:
  bad/service:
    target: http://127.0.0.1:8000
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "invalid service name") {
		t.Fatalf("Load() error = %v, want invalid service name", err)
	}
}
