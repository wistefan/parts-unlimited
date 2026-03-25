package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Gitea.URL == "" {
		t.Error("expected non-empty Gitea URL")
	}
	if cfg.Taiga.URL == "" {
		t.Error("expected non-empty Taiga URL")
	}
	if cfg.Agents.MaxConcurrency != 3 {
		t.Errorf("expected MaxConcurrency=3, got %d", cfg.Agents.MaxConcurrency)
	}
	if cfg.Agents.EscalationThreshold != 2 {
		t.Errorf("expected EscalationThreshold=2, got %d", cfg.Agents.EscalationThreshold)
	}
}

func TestAgentsTimeout(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.IdleTimeout() != 300*time.Second {
		t.Errorf("expected 300s idle timeout, got %v", cfg.Agents.IdleTimeout())
	}
	if cfg.Agents.TaskTimeout() != 3600*time.Second {
		t.Errorf("expected 3600s task timeout, got %v", cfg.Agents.TaskTimeout())
	}
}

func TestLoadFromFile(t *testing.T) {
	content := `
gitea:
  url: "http://localhost:3000"
  adminUsername: "testadmin"
agents:
  maxConcurrency: 5
  specializations:
    test:
      allowedTools: ["Read", "Grep"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if cfg.Gitea.URL != "http://localhost:3000" {
		t.Errorf("expected overridden Gitea URL, got %s", cfg.Gitea.URL)
	}
	if cfg.Gitea.AdminUsername != "testadmin" {
		t.Errorf("expected overridden admin username, got %s", cfg.Gitea.AdminUsername)
	}
	// Default should be preserved for fields not in the file
	if cfg.Gitea.AdminPassword != "password" {
		t.Errorf("expected default admin password preserved, got %s", cfg.Gitea.AdminPassword)
	}
	if cfg.Agents.MaxConcurrency != 5 {
		t.Errorf("expected MaxConcurrency=5, got %d", cfg.Agents.MaxConcurrency)
	}

	testSpec, ok := cfg.Agents.Specializations["test"]
	if !ok {
		t.Fatal("expected test specialization")
	}
	if len(testSpec.AllowedTools) != 2 {
		t.Errorf("expected 2 allowed tools, got %d", len(testSpec.AllowedTools))
	}
}

func TestLoadFromFileMissing(t *testing.T) {
	_, err := LoadFromFile("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}
