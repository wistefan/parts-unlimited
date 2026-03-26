// Package identity manages agent identities across Gitea and Taiga.
package identity

import (
	"fmt"
	"log"
	"sync"

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
	taigaProject  int
	taigaRoleID   int // Role used for agent memberships
	defaultPasswd string

	mu     sync.RWMutex
	agents map[string]*AgentIdentity // keyed by agent ID
	counts map[string]int            // next agent number per specialization
}

// NewManager creates a new identity manager.
func NewManager(giteaClient *gitea.Client, taigaClient *taiga.Client, taigaProjectID, taigaRoleID int) *Manager {
	return &Manager{
		giteaClient:   giteaClient,
		taigaClient:   taigaClient,
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

	// Create Taiga user via public registration, then add as project member.
	taigaUser, err := m.taigaClient.RegisterUser(id, m.defaultPasswd, email, id)
	if err != nil {
		log.Printf("WARNING: Could not register Taiga user %s: %v", id, err)
	}

	taigaUserID := 0
	if taigaUser != nil {
		taigaUserID = taigaUser.ID

		// Add the user as a project member
		membership, err := m.taigaClient.CreateMembership(m.taigaProject, m.taigaRoleID, id)
		if err != nil {
			log.Printf("WARNING: Could not add %s to Taiga project: %v", id, err)
		} else if membership != nil {
			taigaUserID = membership.User
		}
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
