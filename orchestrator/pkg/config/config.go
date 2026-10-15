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
	MaxConcurrency     int    `yaml:"maxConcurrency"`
	IdleTimeoutSeconds int    `yaml:"idleTimeoutSeconds"`
	TaskTimeoutSeconds int    `yaml:"taskTimeoutSeconds"`
	RetryLimit         int    `yaml:"retryLimit"`
	ContainerImage     string `yaml:"containerImage"`

	// Model is the Claude model implementing agents (analysis, plan,
	// step, onestep, fix) run on. Empty means lifecycle's built-in
	// default; review agents are configured separately under Review.
	Model string `yaml:"model"`

	// Effort is the Claude CLI effort level (low, medium, high, xhigh,
	// max) for those same agents. Empty means lifecycle's default.
	Effort string `yaml:"effort"`

	Specializations map[string]SpecConfig `yaml:"specializations"`
	Review          ReviewConfig          `yaml:"review"`
	Credentials     CredentialsConfig     `yaml:"credentials"`
}

// CredentialsConfig holds settings for keeping the Claude OAuth
// credential that agent workers mount from going stale.
//
// Without renewal the Secret is a point-in-time copy of the host's
// credentials file: agents refresh their own copy inside an emptyDir
// that dies with the pod, so the Secret expires and every agent fails
// instantly until setup.sh is run again.
type CredentialsConfig struct {
	// Enabled turns automatic renewal on or off. When false the
	// orchestrator never touches the credential Secret.
	Enabled bool `yaml:"enabled"`

	// SecretName is the Secret holding the credential.
	SecretName string `yaml:"secretName"`

	// RefreshBeforeExpirySeconds is how long before expiry to renew. It
	// must exceed CheckIntervalSeconds, or a token can expire between
	// two checks.
	RefreshBeforeExpirySeconds int `yaml:"refreshBeforeExpirySeconds"`

	// CheckIntervalSeconds is how often the credential is examined.
	CheckIntervalSeconds int `yaml:"checkIntervalSeconds"`
}

// ReviewConfig holds settings for the automated senior-engineer PR
// review agent.
type ReviewConfig struct {
	// Enabled turns automated PR reviews on or off. When false the
	// reconciler never spawns a review agent; every other mode is
	// unaffected.
	Enabled bool `yaml:"enabled"`

	// Model is the Claude model review agents run on, independent of
	// the model the implementing agents use. Empty means lifecycle's
	// built-in default.
	Model string `yaml:"model"`

	// Effort is the review agents' CLI effort level, independent of the
	// implementing agents'. Empty means lifecycle's default.
	Effort string `yaml:"effort"`
}

// SpecConfig holds per-specialization overrides.
type SpecConfig struct {
	AllowedTools []string `yaml:"allowedTools"`
}

// NotificationsConfig holds notification settings.
type NotificationsConfig struct {
	WebhookURL    string `yaml:"webhookUrl"`
	DashboardPort int    `yaml:"dashboardPort"`
	DesktopNotify bool   `yaml:"desktopNotify"`
}

// KubernetesConfig holds Kubernetes-specific settings.
type KubernetesConfig struct {
	Namespace           string `yaml:"namespace"`
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

// Defaults for automatic credential renewal.
const (
	// DefaultCredentialSecretName is the Secret scripts/setup.sh
	// creates from the host's credentials file.
	DefaultCredentialSecretName = "claude-credentials"

	// DefaultRefreshBeforeExpirySeconds renews half an hour ahead of
	// expiry — comfortably more than the check interval below, so a
	// token cannot lapse between two checks.
	DefaultRefreshBeforeExpirySeconds = 1800

	// DefaultCredentialCheckIntervalSeconds is how often the credential
	// is examined. Cheap: a Secret read, and an HTTP call only when the
	// token is actually near expiry.
	DefaultCredentialCheckIntervalSeconds = 300
)

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
			MaxConcurrency:     3,
			IdleTimeoutSeconds: 300,
			TaskTimeoutSeconds: 3600,
			RetryLimit:         2,
			ContainerImage:     "localhost:5000/agent-worker:latest",
			Review:             ReviewConfig{Enabled: true},
			Credentials: CredentialsConfig{
				Enabled:                    true,
				SecretName:                 DefaultCredentialSecretName,
				RefreshBeforeExpirySeconds: DefaultRefreshBeforeExpirySeconds,
				CheckIntervalSeconds:       DefaultCredentialCheckIntervalSeconds,
			},
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
