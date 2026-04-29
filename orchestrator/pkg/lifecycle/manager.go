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

	"github.com/wistefan/dev-env/orchestrator/pkg/metrics"
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

	// DefaultClaudeModel is the Claude model agents use by default.
	DefaultClaudeModel = "claude-opus-4-6"
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
	Namespace          string
	ContainerImage     string
	ServiceAccount     string
	TaskTimeoutSeconds int64
	RetryLimit         int32
	TTLAfterFinished   int32
	ResourceRequests   corev1.ResourceList
	ResourceLimits     corev1.ResourceList
}

// DefaultConfig returns the default lifecycle manager configuration.
func DefaultConfig() *Config {
	return &Config{
		Namespace:          "agents",
		ContainerImage:     "localhost:5000/agent-worker:latest",
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

// trackedJob captures the data needed to emit completion metrics for a Job
// at the moment it first transitions to a terminal state.
type trackedJob struct {
	mode           string
	specialization string
	startTime      time.Time
}

// Manager manages agent worker Kubernetes Jobs.
type Manager struct {
	clientset kubernetes.Interface
	config    *Config

	mu       sync.RWMutex
	tracking map[string]trackedJob // job name -> metrics-tracking data (dedups completion observations)
}

// NewManager creates a new lifecycle manager.
func NewManager(clientset kubernetes.Interface, config *Config) *Manager {
	if config == nil {
		config = DefaultConfig()
	}
	return &Manager{
		clientset: clientset,
		config:    config,
		tracking:  make(map[string]trackedJob),
	}
}

// trackJobCreation records the data needed to emit completion metrics
// later and increments the jobs-created counter.
func (m *Manager) trackJobCreation(jobName, mode, specialization string) {
	m.mu.Lock()
	m.tracking[jobName] = trackedJob{
		mode:           mode,
		specialization: specialization,
		startTime:      time.Now(),
	}
	m.mu.Unlock()
	metrics.JobsCreated.WithLabelValues(mode, specialization).Inc()
}

// observeCompletion emits the completion metrics for a Job at most once.
// Subsequent calls for the same jobName are no-ops. `status` should be one
// of metrics.JobStatus{Succeeded,Failed,Timeout}.
func (m *Manager) observeCompletion(jobName, status string) {
	m.mu.Lock()
	tj, ok := m.tracking[jobName]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.tracking, jobName)
	m.mu.Unlock()

	metrics.JobsCompleted.WithLabelValues(tj.mode, tj.specialization, status).Inc()
	metrics.JobDuration.WithLabelValues(tj.mode, tj.specialization).Observe(time.Since(tj.startTime).Seconds())
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
					// DinD runs as a native sidecar (init container with
					// restartPolicy=Always).  Kubernetes automatically
					// terminates it when the main "agent" container exits,
					// so the Job completes cleanly.
					InitContainers: []corev1.Container{
						{
							Name:  "dind",
							Image: "docker:dind",
							// docker:dind is a public upstream image and rarely
							// changes — no need to pull on every pod start.
							ImagePullPolicy: corev1.PullIfNotPresent,
							RestartPolicy:   func() *corev1.ContainerRestartPolicy { p := corev1.ContainerRestartPolicyAlways; return &p }(),
							SecurityContext: &corev1.SecurityContext{
								// DinD needs root and privileged mode to manage
								// iptables/nftables. The pod-level SecurityContext
								// pins the agent container to UID 1000, so we
								// override RunAsUser/RunAsNonRoot here to prevent
								// the "missing rootlesskit" failure that happens
								// when docker:dind (the non-rootless variant) is
								// forced to run as a non-root user.
								Privileged:   boolPtr(true),
								RunAsUser:    int64Ptr(0),
								RunAsGroup:   int64Ptr(0),
								RunAsNonRoot: boolPtr(false),
							},
							Env: []corev1.EnvVar{
								{
									Name:  "DOCKER_TLS_CERTDIR",
									Value: "", // disable TLS — pod-local communication only
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "docker-sock",
									MountPath: "/var/run",
								},
								{
									Name:      "docker-storage",
									MountPath: "/var/lib/docker",
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "agent",
							Image:           m.config.ContainerImage,
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{
								RunAsUser:                int64Ptr(1000),
								RunAsGroup:               int64Ptr(1000),
								AllowPrivilegeEscalation: boolPtr(true),
							},
							Env: append(m.buildEnvVars(spec), corev1.EnvVar{
								Name:  "DOCKER_HOST",
								Value: "unix:///var/run/docker.sock",
							}),
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "workspace",
									MountPath: "/home/agent/workspace",
								},
								{
									Name:      "claude-home",
									MountPath: "/home/agent/.claude",
								},
								{
									Name:      "claude-credentials",
									MountPath: "/home/agent/.claude-secret",
									ReadOnly:  true,
								},
								{
									Name:      "docker-sock",
									MountPath: "/var/run",
								},
								{
									// Agent transcripts for `npx claude-spend`.
									// bootstrap.sh writes a JSONL per run under
									// <project>/<session>.jsonl; the host directory
									// is shared across all agents.
									Name:      "claude-spend-out",
									MountPath: "/home/agent/claude-spend-out",
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
							Name: "claude-home",
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
						{
							Name: "docker-sock",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "docker-storage",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							// hostPath mirrors the pattern used by Gitea's data
							// volume. Each agent pod writes two files under
							// this directory: its run transcript at
							// `projects/<project>/<session>.jsonl` and an
							// appended entry at `history.jsonl` (required by
							// `npx claude-spend` to render prompt labels).
							// The directory layout deliberately matches what
							// Claude Spend expects inside `~/.claude/`.
							Name: "claude-spend-out",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/dev-env/claude-spend",
									Type: func() *corev1.HostPathType {
										t := corev1.HostPathDirectoryOrCreate
										return &t
									}(),
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

	mode := spec.Mode
	if mode == "" {
		mode = "step"
	}
	m.trackJobCreation(jobName, mode, spec.Specialization)

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
		Name:     job.Name,
		AgentID:  job.Labels[LabelAgentID],
		TicketID: job.Labels[LabelTicketID],
		Active:   job.Status.Active > 0,
	}

	if job.Status.StartTime != nil {
		t := job.Status.StartTime.Time
		status.StartTime = &t
	}

	var failureReason string
	for _, cond := range job.Status.Conditions {
		switch cond.Type {
		case batchv1.JobComplete:
			if cond.Status == corev1.ConditionTrue {
				status.Succeeded = true
			}
		case batchv1.JobFailed:
			if cond.Status == corev1.ConditionTrue {
				status.Failed = true
				failureReason = cond.Reason
			}
		}
	}

	// Emit completion metrics exactly once per job. "DeadlineExceeded" is
	// Kubernetes's reason code when ActiveDeadlineSeconds fires, which is
	// semantically distinct from other failures and worth labeling.
	if status.Succeeded {
		m.observeCompletion(jobName, metrics.JobStatusSucceeded)
	} else if status.Failed {
		if failureReason == "DeadlineExceeded" {
			m.observeCompletion(jobName, metrics.JobStatusTimeout)
		} else {
			m.observeCompletion(jobName, metrics.JobStatusFailed)
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
			Name:     job.Name,
			AgentID:  job.Labels[LabelAgentID],
			TicketID: job.Labels[LabelTicketID],
			Active:   job.Status.Active > 0,
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

// ReapFinishedJobs deletes every agent-worker Job whose status is
// Succeeded or Failed. Returns the number of Jobs deleted.
//
// Kubernetes has its own TTLSecondsAfterFinished GC (300s by default),
// but that delay is too long for the reconcile loop: a completed
// analysis Job must be cleaned up quickly so the reconciler can spawn
// the follow-up plan/onestep Job on the same ticket without tripping
// the HasJobForTicket guard. In authoritative mode the reconciler
// calls this at the start of every pass.
//
// Deletion errors on individual jobs are logged at caller level; this
// method aggregates them into a single returned error if any occurred,
// but still processes the remaining jobs so a transient API error on
// one Job does not block cleanup of the others.
func (m *Manager) ReapFinishedJobs(ctx context.Context) (int, error) {
	jobs, err := m.ListActiveJobs(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	var firstErr error
	for _, j := range jobs {
		if !j.Succeeded && !j.Failed {
			continue
		}
		if err := m.DeleteJob(ctx, j.Name); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("deleting job %s: %w", j.Name, err)
			}
			continue
		}
		count++
	}
	return count, firstErr
}

// ActiveJobCount returns the number of agent-worker Jobs currently
// considered active (Status.Active > 0). This is the Kubernetes-native
// replacement for the in-memory busyAgents counter the assignment
// engine used to maintain; the stateless reconciler calls it each pass
// to enforce the maxConcurrency cap.
//
// Counted per Job, not per ticket: if for any reason two Jobs exist for
// one ticket, both count against the cap. That is the conservative
// choice for a concurrency cap and matches the old "busy pool" semantics.
func (m *Manager) ActiveJobCount(ctx context.Context) (int, error) {
	jobs, err := m.ListActiveJobs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if j.Active {
			n++
		}
	}
	return n, nil
}

// ListActiveAgents returns the set of agent IDs that currently own an
// active Job. The reconciler passes this set to identity.GetOrCreateAgent
// so the identity manager does not hand out the same agent to two
// concurrent tickets. Derived from K8s Job labels — the authoritative
// source — so it survives orchestrator restarts without any persisted
// "busy pool" state.
func (m *Manager) ListActiveAgents(ctx context.Context) (map[string]bool, error) {
	jobs, err := m.ListActiveJobs(ctx)
	if err != nil {
		return nil, err
	}
	busy := make(map[string]bool)
	for _, j := range jobs {
		if j.Active && j.AgentID != "" {
			busy[j.AgentID] = true
		}
	}
	return busy, nil
}

// HasJobForTicket checks whether any Job (running, succeeded, or failed)
// exists for the given ticket, regardless of mode.  This prevents the
// reconciliation loop from spawning duplicate agents when the derived mode
// does not match the mode of the actually running Job.
func (m *Manager) HasJobForTicket(ctx context.Context, ticketID int) (bool, error) {
	ticketLabel := fmt.Sprintf("%s=%d", LabelTicketID, ticketID)
	roleLabel := fmt.Sprintf("%s=%s", LabelRole, LabelRoleValue)
	selector := roleLabel + "," + ticketLabel

	jobs, err := m.clientset.BatchV1().Jobs(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return false, fmt.Errorf("listing jobs for ticket %d: %w", ticketID, err)
	}
	return len(jobs.Items) > 0, nil
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

	envVars = append(envVars, corev1.EnvVar{Name: "CLAUDE_MODEL", Value: DefaultClaudeModel})

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
		corev1.EnvVar{
			Name: "PUSHGATEWAY_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "agent-service-endpoints"},
					Key:                  "PUSHGATEWAY_URL",
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

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
