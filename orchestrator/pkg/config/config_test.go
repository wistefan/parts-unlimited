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

// TestReviewConfigLoading pins two things that are easy to break and
// silent when broken: the shipped ConfigMap actually turns reviews on,
// and a config predating the review feature still gets them (because
// LoadFromFile merges over DefaultConfig rather than replacing it).
func TestReviewConfigLoading(t *testing.T) {
	tests := []struct {
		name            string
		yaml            string
		wantEnabled     bool
		wantReviewTools int
	}{
		{
			name: "agents block as shipped in k8s/agents/orchestrator.yaml",
			yaml: `
agents:
  maxConcurrency: 3
  retryLimit: 2
  review:
    enabled: true
  specializations:
    review:
      allowedTools: ["Read", "Glob", "Grep", "Bash", "Task"]
`,
			wantEnabled:     true,
			wantReviewTools: 5,
		},
		{
			name: "config predating the feature — review still defaults on",
			yaml: `
agents:
  maxConcurrency: 3
  retryLimit: 2
`,
			wantEnabled: true,
		},
		{
			name: "explicitly disabled",
			yaml: `
agents:
  review:
    enabled: false
`,
			wantEnabled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0644); err != nil {
				t.Fatalf("writing config: %v", err)
			}

			cfg, err := LoadFromFile(path)
			if err != nil {
				t.Fatalf("LoadFromFile: %v", err)
			}
			if cfg.Agents.Review.Enabled != tc.wantEnabled {
				t.Errorf("review enabled: got %v, want %v", cfg.Agents.Review.Enabled, tc.wantEnabled)
			}
			if got := len(cfg.Agents.Specializations["review"].AllowedTools); got != tc.wantReviewTools {
				t.Errorf("review allowedTools: got %d, want %d", got, tc.wantReviewTools)
			}
		})
	}
}

func TestCredentialsConfigLoading(t *testing.T) {
	tests := []struct {
		name             string
		yaml             string
		wantEnabled      bool
		wantSecretName   string
		wantRefreshAhead int
		wantInterval     int
	}{
		{
			name:             "defaults when the section is absent",
			yaml:             "agents:\n  maxConcurrency: 3\n",
			wantEnabled:      true,
			wantSecretName:   DefaultCredentialSecretName,
			wantRefreshAhead: DefaultRefreshBeforeExpirySeconds,
			wantInterval:     DefaultCredentialCheckIntervalSeconds,
		},
		{
			name:             "renewal can be turned off",
			yaml:             "agents:\n  credentials:\n    enabled: false\n",
			wantEnabled:      false,
			wantSecretName:   DefaultCredentialSecretName,
			wantRefreshAhead: DefaultRefreshBeforeExpirySeconds,
			wantInterval:     DefaultCredentialCheckIntervalSeconds,
		},
		{
			name: "every field can be overridden",
			yaml: "agents:\n  credentials:\n    enabled: true\n    secretName: \"other-creds\"\n" +
				"    refreshBeforeExpirySeconds: 600\n    checkIntervalSeconds: 60\n",
			wantEnabled:      true,
			wantSecretName:   "other-creds",
			wantRefreshAhead: 600,
			wantInterval:     60,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatalf("writing config: %v", err)
			}

			cfg, err := LoadFromFile(path)
			if err != nil {
				t.Fatalf("loading config: %v", err)
			}

			creds := cfg.Agents.Credentials
			if creds.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", creds.Enabled, tt.wantEnabled)
			}
			if creds.SecretName != tt.wantSecretName {
				t.Errorf("SecretName = %q, want %q", creds.SecretName, tt.wantSecretName)
			}
			if creds.RefreshBeforeExpirySeconds != tt.wantRefreshAhead {
				t.Errorf("RefreshBeforeExpirySeconds = %d, want %d", creds.RefreshBeforeExpirySeconds, tt.wantRefreshAhead)
			}
			if creds.CheckIntervalSeconds != tt.wantInterval {
				t.Errorf("CheckIntervalSeconds = %d, want %d", creds.CheckIntervalSeconds, tt.wantInterval)
			}
		})
	}
}

// The renewal window must exceed the check interval, or a token can
// expire in the gap between two checks — the exact failure this feature
// exists to prevent.
func TestCredentialRenewalWindowExceedsCheckInterval(t *testing.T) {
	creds := DefaultConfig().Agents.Credentials
	if creds.RefreshBeforeExpirySeconds <= creds.CheckIntervalSeconds {
		t.Errorf("refresh window (%ds) must exceed the check interval (%ds)",
			creds.RefreshBeforeExpirySeconds, creds.CheckIntervalSeconds)
	}
}

func TestModelConfigLoading(t *testing.T) {
	// Implementing agents and review agents are configured independently;
	// an empty value means "use the lifecycle package's built-in default",
	// so the loader must not invent one of its own.
	tests := []struct {
		name            string
		yaml            string
		wantModel       string
		wantReviewModel string
	}{
		{
			name: "agents block as shipped in k8s/agents/orchestrator.yaml",
			yaml: `
agents:
  model: "claude-opus-4-8"
  review:
    enabled: true
    model: "claude-opus-5-5"
`,
			wantModel:       "claude-opus-4-8",
			wantReviewModel: "claude-opus-5-5",
		},
		{
			name: "config predating the feature — both empty, defaults apply downstream",
			yaml: `
agents:
  maxConcurrency: 3
`,
			wantModel:       "",
			wantReviewModel: "",
		},
		{
			name: "reviewers can be moved without touching the implementers",
			yaml: `
agents:
  review:
    model: "claude-opus-5-5"
`,
			wantModel:       "",
			wantReviewModel: "claude-opus-5-5",
		},
		{
			name: "implementers can be moved without touching the reviewers",
			yaml: `
agents:
  model: "claude-opus-4-8"
`,
			wantModel:       "claude-opus-4-8",
			wantReviewModel: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0644); err != nil {
				t.Fatalf("writing config: %v", err)
			}

			cfg, err := LoadFromFile(path)
			if err != nil {
				t.Fatalf("LoadFromFile: %v", err)
			}
			if cfg.Agents.Model != tc.wantModel {
				t.Errorf("agents.model: got %q, want %q", cfg.Agents.Model, tc.wantModel)
			}
			if cfg.Agents.Review.Model != tc.wantReviewModel {
				t.Errorf("agents.review.model: got %q, want %q", cfg.Agents.Review.Model, tc.wantReviewModel)
			}
		})
	}
}

func TestEffortConfigLoading(t *testing.T) {
	// Effort mirrors Model: two independent keys, empty meaning "leave the
	// CLI's own per-model default alone".
	tests := []struct {
		name             string
		yaml             string
		wantEffort       string
		wantReviewEffort string
	}{
		{
			name: "agents block as shipped in k8s/agents/orchestrator.yaml",
			yaml: `
agents:
  effort: "high"
  review:
    effort: "high"
`,
			wantEffort:       "high",
			wantReviewEffort: "high",
		},
		{
			name: "config predating the feature — both empty",
			yaml: `
agents:
  maxConcurrency: 3
`,
		},
		{
			name: "reviewers can be raised without touching the implementers",
			yaml: `
agents:
  review:
    effort: "max"
`,
			wantReviewEffort: "max",
		},
		{
			name: "implementers can be lowered without touching the reviewers",
			yaml: `
agents:
  effort: "low"
`,
			wantEffort: "low",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0644); err != nil {
				t.Fatalf("writing config: %v", err)
			}

			cfg, err := LoadFromFile(path)
			if err != nil {
				t.Fatalf("LoadFromFile: %v", err)
			}
			if cfg.Agents.Effort != tc.wantEffort {
				t.Errorf("agents.effort: got %q, want %q", cfg.Agents.Effort, tc.wantEffort)
			}
			if cfg.Agents.Review.Effort != tc.wantReviewEffort {
				t.Errorf("agents.review.effort: got %q, want %q", cfg.Agents.Review.Effort, tc.wantReviewEffort)
			}
		})
	}
}
