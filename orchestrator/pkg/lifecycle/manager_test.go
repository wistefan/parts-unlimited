package lifecycle

import (
	"context"
	"fmt"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestManager() (*Manager, *fake.Clientset) {
	clientset := fake.NewSimpleClientset()
	config := DefaultConfig()
	mgr := NewManager(clientset, config)
	return mgr, clientset
}

func TestJobName(t *testing.T) {
	tests := []struct {
		agentID  string
		ticketID int
		mode     string
		expected string
	}{
		{"general-agent-1", 42, "analysis", "agent-general-agent-1-ticket-42-analysis"},
		{"general-agent-1", 42, "plan", "agent-general-agent-1-ticket-42-plan"},
		{"frontend-agent-3", 100, "step", "agent-frontend-agent-3-ticket-100-step"},
		{"general-agent-1", 42, "", "agent-general-agent-1-ticket-42-step"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := JobName(tt.agentID, tt.ticketID, tt.mode)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestCreateJob(t *testing.T) {
	mgr, clientset := newTestManager()
	ctx := context.Background()

	// Create the agents namespace
	clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "agents"},
	}, metav1.CreateOptions{})

	spec := &AgentJobSpec{
		AgentID:        "general-agent-1",
		Specialization: "general",
		TicketID:       42,
		Mode:           "step",
		PlanStep:       "3",
		RepoOwner:      "claude",
		RepoName:       "test-repo",
		GiteaUsername:  "general-agent-1",
		GiteaPassword:  "agent-password",
		TaigaUsername:  "general-agent-1",
		TaigaPassword:  "agent-password",
	}

	jobName, err := mgr.CreateJob(ctx, spec)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	expectedName := "agent-general-agent-1-ticket-42-step"
	if jobName != expectedName {
		t.Errorf("expected job name %q, got %q", expectedName, jobName)
	}

	// Verify job was created
	job, err := clientset.BatchV1().Jobs("agents").Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get job: %v", err)
	}

	// Check labels
	if job.Labels[LabelRole] != LabelRoleValue {
		t.Errorf("expected label %s=%s, got %s", LabelRole, LabelRoleValue, job.Labels[LabelRole])
	}
	if job.Labels[LabelAgentID] != "general-agent-1" {
		t.Errorf("expected agent-id label, got %s", job.Labels[LabelAgentID])
	}
	if job.Labels[LabelTicketID] != "42" {
		t.Errorf("expected ticket-id label '42', got %s", job.Labels[LabelTicketID])
	}

	// Check security context
	podSpec := job.Spec.Template.Spec
	if podSpec.SecurityContext == nil || *podSpec.SecurityContext.RunAsNonRoot != true {
		t.Error("expected RunAsNonRoot=true")
	}
	if *podSpec.SecurityContext.RunAsUser != 1000 {
		t.Errorf("expected RunAsUser=1000, got %d", *podSpec.SecurityContext.RunAsUser)
	}

	// Check container
	container := podSpec.Containers[0]
	if container.Image != "localhost:5000/agent-worker:latest" {
		t.Errorf("expected image 'localhost:5000/agent-worker:latest', got %s", container.Image)
	}
	// AllowPrivilegeEscalation is intentionally true: the agent image grants
	// the agent user passwordless sudo so it can install project-specific
	// toolchains at runtime (see agent/Dockerfile). The container is still
	// pinned to non-root startup via the pod-level SecurityContext and is
	// network-isolated by NetworkPolicy, so escalation inside the pod carries
	// no cross-pod blast radius.
	if *container.SecurityContext.AllowPrivilegeEscalation != true {
		t.Error("expected AllowPrivilegeEscalation=true (required for sudo-based toolchain install)")
	}

	// Check service account
	if podSpec.ServiceAccountName != "agent-worker" {
		t.Errorf("expected ServiceAccountName 'agent-worker', got %s", podSpec.ServiceAccountName)
	}
	if *podSpec.AutomountServiceAccountToken != false {
		t.Error("expected AutomountServiceAccountToken=false")
	}

	// Check env vars
	envMap := make(map[string]corev1.EnvVar)
	for _, env := range container.Env {
		envMap[env.Name] = env
	}
	if envMap["TICKET_ID"].Value != "42" {
		t.Errorf("expected TICKET_ID=42, got %s", envMap["TICKET_ID"].Value)
	}
	if envMap["AGENT_ID"].Value != "general-agent-1" {
		t.Errorf("expected AGENT_ID='general-agent-1', got %s", envMap["AGENT_ID"].Value)
	}
	if envMap["PLAN_STEP"].Value != "3" {
		t.Errorf("expected PLAN_STEP=3, got %s", envMap["PLAN_STEP"].Value)
	}
	// ANTHROPIC_API_KEY should be optional from secret
	apiKeyEnv := envMap["ANTHROPIC_API_KEY"]
	if apiKeyEnv.ValueFrom == nil || apiKeyEnv.ValueFrom.SecretKeyRef == nil {
		t.Error("expected ANTHROPIC_API_KEY from secret")
	} else if apiKeyEnv.ValueFrom.SecretKeyRef.Optional == nil || !*apiKeyEnv.ValueFrom.SecretKeyRef.Optional {
		t.Error("expected ANTHROPIC_API_KEY secret ref to be optional")
	}
	// Claude credentials should be mounted as a volume
	var hasCredentialsMount bool
	for _, vm := range container.VolumeMounts {
		if vm.Name == "claude-home" && vm.MountPath == "/home/agent/.claude" {
			hasCredentialsMount = true
			break
		}
	}
	if !hasCredentialsMount {
		t.Error("expected claude-home volume mount at /home/agent/.claude")
	}
	var hasCredentialsVolume bool
	for _, v := range podSpec.Volumes {
		if v.Name == "claude-credentials" && v.Secret != nil && v.Secret.SecretName == "claude-credentials" {
			hasCredentialsVolume = true
			break
		}
	}
	if !hasCredentialsVolume {
		t.Error("expected claude-credentials volume from secret")
	}
	// GITEA_URL should come from configmap
	if envMap["GITEA_URL"].ValueFrom == nil || envMap["GITEA_URL"].ValueFrom.ConfigMapKeyRef == nil {
		t.Error("expected GITEA_URL from configmap")
	}

	// Verify the Job exists in the K8s API — this is the source of
	// truth, not any in-memory tracking.
	if _, err := mgr.clientset.BatchV1().Jobs("agents").Get(context.Background(), jobName, metav1.GetOptions{}); err != nil {
		t.Errorf("expected job %s to exist in K8s, got: %v", jobName, err)
	}
}

func TestCreateJob_Idempotent(t *testing.T) {
	mgr, clientset := newTestManager()
	ctx := context.Background()

	clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "agents"},
	}, metav1.CreateOptions{})

	spec := &AgentJobSpec{
		AgentID:        "general-agent-1",
		Specialization: "general",
		TicketID:       42,
		GiteaUsername:   "general-agent-1",
		GiteaPassword:   "pass",
		TaigaUsername:   "general-agent-1",
		TaigaPassword:   "pass",
	}

	name1, err := mgr.CreateJob(ctx, spec)
	if err != nil {
		t.Fatalf("first CreateJob: %v", err)
	}

	// Creating again should not fail
	name2, err := mgr.CreateJob(ctx, spec)
	if err != nil {
		t.Fatalf("second CreateJob: %v", err)
	}

	if name1 != name2 {
		t.Errorf("expected same job name, got %q and %q", name1, name2)
	}
}

func TestDeleteJob(t *testing.T) {
	mgr, clientset := newTestManager()
	ctx := context.Background()

	clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "agents"},
	}, metav1.CreateOptions{})

	spec := &AgentJobSpec{
		AgentID:        "general-agent-1",
		Specialization: "general",
		TicketID:       42,
		GiteaUsername:   "a",
		GiteaPassword:   "a",
		TaigaUsername:   "a",
		TaigaPassword:   "a",
	}
	jobName, _ := mgr.CreateJob(ctx, spec)

	err := mgr.DeleteJob(ctx, jobName)
	if err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	// The Job should no longer exist in the K8s API — that is the
	// only source of truth.
	if _, err := mgr.clientset.BatchV1().Jobs("agents").Get(ctx, jobName, metav1.GetOptions{}); !errors.IsNotFound(err) {
		t.Errorf("expected job %s to be gone from K8s, got: %v", jobName, err)
	}
}

func TestDeleteJob_NotFound(t *testing.T) {
	mgr, _ := newTestManager()
	ctx := context.Background()

	// Deleting a non-existent job should not error
	err := mgr.DeleteJob(ctx, "nonexistent-job")
	if err != nil {
		t.Errorf("expected no error for missing job, got %v", err)
	}
}

func TestGetJobStatus(t *testing.T) {
	mgr, clientset := newTestManager()
	ctx := context.Background()

	clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "agents"},
	}, metav1.CreateOptions{})

	// Create a job directly in the fake clientset with a completed status
	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "agents",
			Labels: map[string]string{
				LabelAgentID:  "test-agent-1",
				LabelTicketID: "99",
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	clientset.BatchV1().Jobs("agents").Create(ctx, completedJob, metav1.CreateOptions{})

	status, err := mgr.GetJobStatus(ctx, "test-job")
	if err != nil {
		t.Fatalf("GetJobStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected non-nil status")
	}
	if !status.Succeeded {
		t.Error("expected succeeded=true")
	}
	if status.AgentID != "test-agent-1" {
		t.Errorf("expected agent-id 'test-agent-1', got %q", status.AgentID)
	}
}

func TestGetJobStatus_NotFound(t *testing.T) {
	mgr, _ := newTestManager()
	ctx := context.Background()

	status, err := mgr.GetJobStatus(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetJobStatus: %v", err)
	}
	if status != nil {
		t.Error("expected nil status for missing job")
	}
}

func TestListActiveJobs(t *testing.T) {
	mgr, clientset := newTestManager()
	ctx := context.Background()

	clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "agents"},
	}, metav1.CreateOptions{})

	// Create two agent jobs
	for i := 1; i <= 2; i++ {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("agent-job-%d", i),
				Namespace: "agents",
				Labels: map[string]string{
					LabelRole:     LabelRoleValue,
					LabelAgentID:  fmt.Sprintf("agent-%d", i),
					LabelTicketID: fmt.Sprintf("%d", i),
				},
			},
			Status: batchv1.JobStatus{Active: 1},
		}
		clientset.BatchV1().Jobs("agents").Create(ctx, job, metav1.CreateOptions{})
	}

	// Create a non-agent job (should be excluded)
	otherJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-job",
			Namespace: "agents",
			Labels:    map[string]string{"role": "other"},
		},
	}
	clientset.BatchV1().Jobs("agents").Create(ctx, otherJob, metav1.CreateOptions{})

	statuses, err := mgr.ListActiveJobs(ctx)
	if err != nil {
		t.Fatalf("ListActiveJobs: %v", err)
	}
	if len(statuses) != 2 {
		t.Errorf("expected 2 agent jobs, got %d", len(statuses))
	}
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()
	if config.Namespace != "agents" {
		t.Errorf("expected namespace 'agents', got %q", config.Namespace)
	}
	if config.TaskTimeoutSeconds != 3600 {
		t.Errorf("expected timeout 3600, got %d", config.TaskTimeoutSeconds)
	}
	if config.RetryLimit != 2 {
		t.Errorf("expected retry limit 2, got %d", config.RetryLimit)
	}
	if config.TTLAfterFinished != DefaultTTLAfterFinished {
		t.Errorf("expected TTL %d, got %d", DefaultTTLAfterFinished, config.TTLAfterFinished)
	}
}
