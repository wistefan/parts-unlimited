// Package config provides configuration structures for the orchestrator.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level orchestrator configuration.
type Config struct {
	Gitea         GiteaConfig         `yaml:"gitea"`
	Taiga         TaigaConfig         `yaml:"taiga"`
	Agents        AgentsConfig        `yaml:"agents"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Kubernetes    KubernetesConfig    `yaml:"kubernetes"`
}

// GiteaConfig holds Gitea connection settings.
type GiteaConfig struct {
	URL           string `yaml:"url"`
	AdminUsername string `yaml:"adminUsername"`
	AdminPassword string `yaml:"adminPassword"`
}

// TaigaConfig holds Taiga connection settings.
type TaigaConfig struct {
	URL           string `yaml:"url"`
	AdminUsername string `yaml:"adminUsername"`
	AdminPassword string `yaml:"adminPassword"`
	ProjectSlug   string `yaml:"projectSlug"`
	WebhookSecret string `yaml:"webhookSecret"`
	HumanUsername string `yaml:"humanUsername"`
}

// AgentsConfig holds agent orchestration settings.
type AgentsConfig struct {
	MaxConcurrency      int                   `yaml:"maxConcurrency"`
	IdleTimeoutSeconds  int                   `yaml:"idleTimeoutSeconds"`
	TaskTimeoutSeconds  int                   `yaml:"taskTimeoutSeconds"`
	RetryLimit          int                   `yaml:"retryLimit"`
	EscalationThreshold int                   `yaml:"escalationThreshold"`
	ContainerImage      string                `yaml:"containerImage"`
	Specializations     map[string]SpecConfig `yaml:"specializations"`
}

// SpecConfig holds per-specialization overrides.
type SpecConfig struct {
	AllowedTools []string `yaml:"allowedTools"`
}

// NotificationsConfig holds notification settings.
type NotificationsConfig struct {
	WebhookURL     string `yaml:"webhookUrl"`
	DashboardPort  int    `yaml:"dashboardPort"`
	DesktopNotify  bool   `yaml:"desktopNotify"`
}

// KubernetesConfig holds Kubernetes-specific settings.
type KubernetesConfig struct {
	Namespace         string `yaml:"namespace"`
	AgentServiceAccount string `yaml:"agentServiceAccount"`
}

// IdleTimeout returns the idle timeout as a time.Duration.
func (a *AgentsConfig) IdleTimeout() time.Duration {
	return time.Duration(a.IdleTimeoutSeconds) * time.Second
}

// TaskTimeout returns the task timeout as a time.Duration.
func (a *AgentsConfig) TaskTimeout() time.Duration {
	return time.Duration(a.TaskTimeoutSeconds) * time.Second
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		Gitea: GiteaConfig{
			URL:           "http://gitea-http.gitea.svc.cluster.local:3001",
			AdminUsername: "claude",
			AdminPassword: "password",
		},
		Taiga: TaigaConfig{
			URL:           "http://taiga-gateway.taiga.svc.cluster.local:9000",
			AdminUsername: "admin",
			AdminPassword: "password",
			ProjectSlug:   "dev-environment",
			HumanUsername: "wistefan",
		},
		Agents: AgentsConfig{
			MaxConcurrency:      3,
			IdleTimeoutSeconds:  300,
			TaskTimeoutSeconds:  3600,
			RetryLimit:          2,
			EscalationThreshold: 2,
			ContainerImage:      "localhost:5000/agent-worker:latest",
		},
		Notifications: NotificationsConfig{
			DashboardPort: 8080,
			DesktopNotify: false,
		},
		Kubernetes: KubernetesConfig{
			Namespace:           "agents",
			AgentServiceAccount: "agent-worker",
		},
	}
}

// LoadFromFile reads a YAML config file and merges it with defaults.
func LoadFromFile(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	return cfg, nil
}
