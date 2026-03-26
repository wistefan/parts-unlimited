// Package identity manages agent identities across Gitea and Taiga.
package identity

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
	"github.com/wistefan/dev-env/orchestrator/pkg/taiga"
)

// DefaultSpecializations is the starting set of agent specializations.
var DefaultSpecializations = []string{
	"general",
	"frontend",
	"backend",
	"test",
	"documentation",
	"operations",
}

// AgentIdentity represents a registered agent across Gitea and Taiga.
type AgentIdentity struct {
	ID             string `json:"id"`             // e.g., "frontend-agent-1"
	Specialization string `json:"specialization"` // e.g., "frontend"
	GiteaUserID    int    `json:"giteaUserId"`
	TaigaUserID    int    `json:"taigaUserId"`
	Password       string `json:"password"`
	Email          string `json:"email"`
}

// Manager handles creation, lookup, and lifecycle of agent identities.
type Manager struct {
	giteaClient   *gitea.Client
	taigaClient   *taiga.Client
	k8sClient     kubernetes.Interface
	k8sConfig     *rest.Config
	taigaProject  int
	taigaRoleID   int // Role used for agent memberships
	defaultPasswd string

	mu     sync.RWMutex
	agents map[string]*AgentIdentity // keyed by agent ID
	counts map[string]int            // next agent number per specialization
}

// NewManager creates a new identity manager.
func NewManager(giteaClient *gitea.Client, taigaClient *taiga.Client, k8sClient kubernetes.Interface, k8sConfig *rest.Config, taigaProjectID, taigaRoleID int) *Manager {
	return &Manager{
		giteaClient:   giteaClient,
		taigaClient:   taigaClient,
		k8sClient:     k8sClient,
		k8sConfig:     k8sConfig,
		taigaProject:  taigaProjectID,
		taigaRoleID:   taigaRoleID,
		defaultPasswd: "agent-password",
		agents:        make(map[string]*AgentIdentity),
		counts:        make(map[string]int),
	}
}

// AgentID generates the agent ID for a given specialization and number.
func AgentID(specialization string, number int) string {
	return fmt.Sprintf("%s-agent-%d", specialization, number)
}

// GetOrCreateAgent returns an existing idle agent of the given specialization,
// or creates a new one if none are available.
func (m *Manager) GetOrCreateAgent(specialization string, busyAgents map[string]bool) (*AgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Look for an existing idle agent of this specialization
	for _, agent := range m.agents {
		if agent.Specialization == specialization && !busyAgents[agent.ID] {
			log.Printf("Reusing existing agent: %s", agent.ID)
			return agent, nil
		}
	}

	// Create a new agent
	return m.createAgentLocked(specialization)
}

// createAgentLocked creates a new agent identity. Must be called with mu held.
func (m *Manager) createAgentLocked(specialization string) (*AgentIdentity, error) {
	m.counts[specialization]++
	number := m.counts[specialization]

	id := AgentID(specialization, number)
	email := fmt.Sprintf("%s@dev-env.local", id)

	log.Printf("Creating agent identity: %s", id)

	// Create Gitea user
	giteaUser, err := m.giteaClient.CreateUser(id, m.defaultPasswd, email)
	if err != nil {
		return nil, fmt.Errorf("creating Gitea user %s: %w", id, err)
	}

	// Create Taiga user and project membership via Django ORM.
	// The Taiga REST API requires users to be "contacts" before they can be
	// added as project members, which doesn't work on fresh setups. We bypass
	// this by execing into the taiga-back pod and using the ORM directly.
	taigaUserID, err := m.createTaigaMembership(id, m.defaultPasswd, email)
	if err != nil {
		log.Printf("WARNING: Could not create Taiga user/membership for %s: %v", id, err)
	}

	agent := &AgentIdentity{
		ID:             id,
		Specialization: specialization,
		GiteaUserID:    giteaUser.ID,
		TaigaUserID:    taigaUserID,
		Password:       m.defaultPasswd,
		Email:          email,
	}

	m.agents[id] = agent
	log.Printf("Agent identity created: %s (gitea=%d, taiga=%d)", id, giteaUser.ID, taigaUserID)

	return agent, nil
}

// GetAgent returns a registered agent by ID, or nil if not found.
func (m *Manager) GetAgent(id string) *AgentIdentity {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agents[id]
}

// ListAgents returns all registered agents.
func (m *Manager) ListAgents() []*AgentIdentity {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*AgentIdentity, 0, len(m.agents))
	for _, a := range m.agents {
		result = append(result, a)
	}
	return result
}

// ListBySpecialization returns all agents of a given specialization.
func (m *Manager) ListBySpecialization(specialization string) []*AgentIdentity {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*AgentIdentity
	for _, a := range m.agents {
		if a.Specialization == specialization {
			result = append(result, a)
		}
	}
	return result
}

// RegisterExisting adds a pre-existing agent identity to the manager.
// Used during recovery to load agents from persisted state.
func (m *Manager) RegisterExisting(agent *AgentIdentity) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.agents[agent.ID] = agent

	// Update the count so new agents get the next number
	// Parse the number from the ID to keep counts in sync
	var num int
	var spec string
	_, err := fmt.Sscanf(agent.ID, "%s", &spec)
	if err == nil {
		// Extract number from "specialization-agent-N"
		fmt.Sscanf(agent.ID, agent.Specialization+"-agent-%d", &num)
		if num >= m.counts[agent.Specialization] {
			m.counts[agent.Specialization] = num
		}
	}
}

// RemoveAgent deactivates an agent identity.
// Note: does not delete Gitea/Taiga users (they may have authored commits/comments).
func (m *Manager) RemoveAgent(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.agents, id)
}

// createTaigaMembership creates a Taiga user and adds them to the project
// by execing into the taiga-back pod and using the Django ORM directly.
// Returns the Taiga user ID.
func (m *Manager) createTaigaMembership(username, password, email string) (int, error) {
	if m.k8sClient == nil || m.k8sConfig == nil {
		// Fall back to REST API when K8s access is unavailable (e.g., in tests)
		membership, err := m.taigaClient.CreateMembership(m.taigaProject, m.taigaRoleID, username)
		if err != nil {
			return 0, err
		}
		return membership.User, nil
	}

	ctx := context.Background()

	// Find the taiga-back pod
	pods, err := m.k8sClient.CoreV1().Pods("taiga").List(ctx, metav1.ListOptions{
		LabelSelector: "app=taiga-back",
	})
	if err != nil {
		return 0, fmt.Errorf("listing taiga-back pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return 0, fmt.Errorf("no taiga-back pods found")
	}
	podName := pods.Items[0].Name

	// Python script to create user and membership via Django ORM
	script := fmt.Sprintf(`
from django.contrib.auth import get_user_model
from taiga.projects.models import Project, Membership

User = get_user_model()
user, created = User.objects.get_or_create(
    username='%s',
    defaults={'email': '%s', 'is_active': True}
)
if created:
    user.set_password('%s')
    user.save()

project = Project.objects.get(id=%d)
role = max(project.roles.all(), key=lambda r: len(r.permissions or []))
membership, m_created = Membership.objects.get_or_create(
    project=project, user=user,
    defaults={'role': role, 'is_admin': False}
)
print(user.id)
`, username, email, password, m.taigaProject)

	// Exec into the pod
	req := m.k8sClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace("taiga").
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{"python", "manage.py", "shell", "-c", script},
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(m.k8sConfig, "POST", req.URL())
	if err != nil {
		return 0, fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return 0, fmt.Errorf("exec failed: %w, stderr: %s", err, stderr.String())
	}

	// Parse the user ID from stdout
	var userID int
	if _, err := fmt.Sscanf(stdout.String(), "%d", &userID); err != nil {
		return 0, fmt.Errorf("parsing user ID from output %q: %w", stdout.String(), err)
	}

	log.Printf("Taiga user %s created/found (ID: %d) with project membership", username, userID)
	return userID, nil
}
