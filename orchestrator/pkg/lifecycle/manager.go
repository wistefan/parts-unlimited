// Package lifecycle manages the creation, monitoring, and destruction
// of agent worker Kubernetes Jobs.
package lifecycle

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// LabelRole identifies agent worker pods for network policies.
	LabelRole = "role"

	// LabelRoleValue is the label value for agent worker pods.
	LabelRoleValue = "agent-worker"

	// LabelAgentID labels jobs with the agent identity.
	LabelAgentID = "agent-id"

	// LabelTicketID labels jobs with the ticket being worked on.
	LabelTicketID = "ticket-id"

	// DefaultTTLAfterFinished is how long completed Jobs are kept for log access.
	DefaultTTLAfterFinished int32 = 300
)

// AgentJobSpec holds the parameters needed to create an agent worker Job.
type AgentJobSpec struct {
	AgentID        string
	Specialization string
	TicketID       int
	Mode           string // analysis, plan, step, fix
	PlanStep       string
	RepoOwner      string
	RepoName       string
	PRNumber       int    // PR to fix (fix mode only)
	PRRepo         string // "{owner}/{repo}" of the PR (fix mode only)
	GiteaUsername  string
	GiteaPassword  string
	TaigaUsername  string
	TaigaPassword  string
	AllowedTools   string
	HumanUsername  string
	HumanTaigaID   int
	TaigaProjectID int
}

// Config holds lifecycle manager configuration.
type Config struct {
	Namespace           string
	ContainerImage      string
	ServiceAccount      string
	TaskTimeoutSeconds  int64
	RetryLimit          int32
	TTLAfterFinished    int32
	ResourceRequests    corev1.ResourceList
	ResourceLimits      corev1.ResourceList
}

// DefaultConfig returns the default lifecycle manager configuration.
func DefaultConfig() *Config {
	return &Config{
		Namespace:          "agents",
		ContainerImage:     "agent-worker:latest",
		ServiceAccount:     "agent-worker",
		TaskTimeoutSeconds: 3600,
		RetryLimit:         2,
		TTLAfterFinished:   DefaultTTLAfterFinished,
	}
}

// JobStatus represents the observed status of an agent Job.
type JobStatus struct {
	Name      string
	AgentID   string
	TicketID  string
	Active    bool
	Succeeded bool
	Failed    bool
	StartTime *time.Time
}

// Manager manages agent worker Kubernetes Jobs.
type Manager struct {
	clientset kubernetes.Interface
	config    *Config

	mu       sync.RWMutex
	activeJobs map[string]string // job name -> agent ID
}

// NewManager creates a new lifecycle manager.
func NewManager(clientset kubernetes.Interface, config *Config) *Manager {
	if config == nil {
		config = DefaultConfig()
	}
	return &Manager{
		clientset:  clientset,
		config:     config,
		activeJobs: make(map[string]string),
	}
}

// JobName generates a deterministic job name for an agent/ticket/mode combination.
func JobName(agentID string, ticketID int, mode string) string {
	if mode == "" {
		mode = "step"
	}
	return fmt.Sprintf("agent-%s-ticket-%d-%s", agentID, ticketID, mode)
}

// CreateJob creates a Kubernetes Job for an agent worker.
func (m *Manager) CreateJob(ctx context.Context, spec *AgentJobSpec) (string, error) {
	jobName := JobName(spec.AgentID, spec.TicketID, spec.Mode)

	// Check if job already exists
	_, err := m.clientset.BatchV1().Jobs(m.config.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err == nil {
		log.Printf("Job %s already exists, skipping creation", jobName)
		return jobName, nil
	}
	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("checking existing job: %w", err)
	}

	ttl := m.config.TTLAfterFinished
	deadline := m.config.TaskTimeoutSeconds
	backoff := m.config.RetryLimit
	falseVal := false

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: m.config.Namespace,
			Labels: map[string]string{
				LabelRole:     LabelRoleValue,
				LabelAgentID:  spec.AgentID,
				LabelTicketID: fmt.Sprintf("%d", spec.TicketID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelRole:     LabelRoleValue,
						LabelAgentID:  spec.AgentID,
						LabelTicketID: fmt.Sprintf("%d", spec.TicketID),
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:           m.config.ServiceAccount,
					AutomountServiceAccountToken: &falseVal,
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    int64Ptr(1000),
						RunAsGroup:   int64Ptr(1000),
						FSGroup:      int64Ptr(1000),
					},
					Containers: []corev1.Container{
						{
							Name:            "agent",
							Image:           m.config.ContainerImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: &falseVal,
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							Env: m.buildEnvVars(spec),
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "workspace",
									MountPath: "/home/agent/workspace",
								},
								{
									Name:      "claude-credentials",
									MountPath: "/home/agent/.claude",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "claude-credentials",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "claude-credentials",
									Items: []corev1.KeyToPath{
										{
											Key:  "credentials.json",
											Path: ".credentials.json",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Apply resource limits if configured
	if m.config.ResourceRequests != nil || m.config.ResourceLimits != nil {
		job.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: m.config.ResourceRequests,
			Limits:   m.config.ResourceLimits,
		}
	}

	created, err := m.clientset.BatchV1().Jobs(m.config.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating job %s: %w", jobName, err)
	}

	m.mu.Lock()
	m.activeJobs[jobName] = spec.AgentID
	m.mu.Unlock()

	log.Printf("Created job %s for agent %s on ticket %d", created.Name, spec.AgentID, spec.TicketID)
	return created.Name, nil
}

// DeleteJob deletes a Kubernetes Job and its pods.
func (m *Manager) DeleteJob(ctx context.Context, jobName string) error {
	propagation := metav1.DeletePropagationForeground
	err := m.clientset.BatchV1().Jobs(m.config.Namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting job %s: %w", jobName, err)
	}

	m.mu.Lock()
	delete(m.activeJobs, jobName)
	m.mu.Unlock()

	log.Printf("Deleted job %s", jobName)
	return nil
}

// GetJobStatus retrieves the status of a job.
func (m *Manager) GetJobStatus(ctx context.Context, jobName string) (*JobStatus, error) {
	job, err := m.clientset.BatchV1().Jobs(m.config.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting job %s: %w", jobName, err)
	}

	status := &JobStatus{
		Name:    job.Name,
		AgentID: job.Labels[LabelAgentID],
		TicketID: job.Labels[LabelTicketID],
		Active:  job.Status.Active > 0,
	}

	if job.Status.StartTime != nil {
		t := job.Status.StartTime.Time
		status.StartTime = &t
	}

	for _, cond := range job.Status.Conditions {
		switch cond.Type {
		case batchv1.JobComplete:
			if cond.Status == corev1.ConditionTrue {
				status.Succeeded = true
			}
		case batchv1.JobFailed:
			if cond.Status == corev1.ConditionTrue {
				status.Failed = true
			}
		}
	}

	return status, nil
}

// ListActiveJobs returns all agent worker jobs in the namespace.
func (m *Manager) ListActiveJobs(ctx context.Context) ([]JobStatus, error) {
	labelSelector := fmt.Sprintf("%s=%s", LabelRole, LabelRoleValue)

	jobs, err := m.clientset.BatchV1().Jobs(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}

	var result []JobStatus
	for _, job := range jobs.Items {
		status := JobStatus{
			Name:    job.Name,
			AgentID: job.Labels[LabelAgentID],
			TicketID: job.Labels[LabelTicketID],
			Active:  job.Status.Active > 0,
		}

		if job.Status.StartTime != nil {
			t := job.Status.StartTime.Time
			status.StartTime = &t
		}

		for _, cond := range job.Status.Conditions {
			switch cond.Type {
			case batchv1.JobComplete:
				if cond.Status == corev1.ConditionTrue {
					status.Succeeded = true
				}
			case batchv1.JobFailed:
				if cond.Status == corev1.ConditionTrue {
					status.Failed = true
				}
			}
		}

		result = append(result, status)
	}

	return result, nil
}

// GetActiveJobNames returns the set of currently tracked active job names.
func (m *Manager) GetActiveJobNames() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]string, len(m.activeJobs))
	for k, v := range m.activeJobs {
		result[k] = v
	}
	return result
}

// buildEnvVars constructs the environment variables for an agent container.
func (m *Manager) buildEnvVars(spec *AgentJobSpec) []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{Name: "TICKET_ID", Value: fmt.Sprintf("%d", spec.TicketID)},
		{Name: "AGENT_ID", Value: spec.AgentID},
		{Name: "AGENT_SPECIALIZATION", Value: spec.Specialization},
		{Name: "MODE", Value: spec.Mode},
		{Name: "PLAN_STEP", Value: spec.PlanStep},
		{Name: "REPO_OWNER", Value: spec.RepoOwner},
		{Name: "REPO_NAME", Value: spec.RepoName},
		{Name: "GITEA_USERNAME", Value: spec.GiteaUsername},
		{Name: "GITEA_PASSWORD", Value: spec.GiteaPassword},
		{Name: "TAIGA_USERNAME", Value: spec.TaigaUsername},
		{Name: "TAIGA_PASSWORD", Value: spec.TaigaPassword},
		{Name: "HUMAN_USERNAME", Value: spec.HumanUsername},
		{Name: "HUMAN_TAIGA_ID", Value: fmt.Sprintf("%d", spec.HumanTaigaID)},
		{Name: "TAIGA_PROJECT_ID", Value: fmt.Sprintf("%d", spec.TaigaProjectID)},
	}

	if spec.Mode == "fix" && spec.PRNumber > 0 {
		envVars = append(envVars,
			corev1.EnvVar{Name: "PR_NUMBER", Value: fmt.Sprintf("%d", spec.PRNumber)},
			corev1.EnvVar{Name: "PR_REPO", Value: spec.PRRepo},
		)
	}

	if spec.AllowedTools != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "ALLOWED_TOOLS", Value: spec.AllowedTools})
	}

	// Service endpoints from ConfigMap
	envVars = append(envVars,
		corev1.EnvVar{
			Name: "GITEA_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "agent-service-endpoints"},
					Key:                  "GITEA_URL",
				},
			},
		},
		corev1.EnvVar{
			Name: "TAIGA_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "agent-service-endpoints"},
					Key:                  "TAIGA_URL",
				},
			},
		},
	)

	// Optional: if the anthropic-api-key Secret exists, it takes
	// precedence over the mounted credentials file.
	optional := true
	envVars = append(envVars, corev1.EnvVar{
		Name: "ANTHROPIC_API_KEY",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "anthropic-api-key"},
				Key:                  "api-key",
				Optional:             &optional,
			},
		},
	})

	return envVars
}

func boolPtr(b bool) *bool       { return &b }
func int64Ptr(i int64) *int64    { return &i }
