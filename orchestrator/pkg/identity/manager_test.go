package identity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
	"github.com/wistefan/dev-env/orchestrator/pkg/taiga"
)

func setupTestManager(t *testing.T) (*Manager, *httptest.Server, *httptest.Server) {
	t.Helper()

	var giteaUserCounter int32

	giteaMux := http.NewServeMux()
	giteaMux.HandleFunc("/api/v1/admin/users", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		id := int(atomic.AddInt32(&giteaUserCounter, 1))
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(gitea.GiteaUser{
			ID:    id,
			Login: body["username"].(string),
			Email: body["email"].(string),
		})
	})
	giteaSrv := httptest.NewServer(giteaMux)
	t.Cleanup(giteaSrv.Close)

	taigaMux := http.NewServeMux()
	taigaMux.HandleFunc("/api/v1/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"auth_token": "test-token"})
	})
	taigaMux.HandleFunc("/api/v1/memberships", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(taiga.Membership{ID: 1, User: 100, Project: 1, Role: 1})
	})
	taigaSrv := httptest.NewServer(taigaMux)
	t.Cleanup(taigaSrv.Close)

	giteaClient := gitea.NewClient(giteaSrv.URL, "admin", "password")
	taigaClient := taiga.NewClient(taigaSrv.URL)
	taigaClient.Authenticate("admin", "password")

	manager := NewManager(giteaClient, taigaClient, nil, nil, 1, 1)

	return manager, giteaSrv, taigaSrv
}

func TestAgentID(t *testing.T) {
	tests := []struct {
		spec   string
		num    int
		expect string
	}{
		{"general", 1, "general-agent-1"},
		{"frontend", 3, "frontend-agent-3"},
		{"test", 10, "test-agent-10"},
	}

	for _, tt := range tests {
		t.Run(tt.expect, func(t *testing.T) {
			result := AgentID(tt.spec, tt.num)
			if result != tt.expect {
				t.Errorf("expected %q, got %q", tt.expect, result)
			}
		})
	}
}

func TestGetOrCreateAgent_CreatesNew(t *testing.T) {
	mgr, _, _ := setupTestManager(t)

	agent, err := mgr.GetOrCreateAgent("frontend", map[string]bool{})
	if err != nil {
		t.Fatalf("GetOrCreateAgent: %v", err)
	}

	if agent.ID != "frontend-agent-1" {
		t.Errorf("expected ID 'frontend-agent-1', got %q", agent.ID)
	}
	if agent.Specialization != "frontend" {
		t.Errorf("expected specialization 'frontend', got %q", agent.Specialization)
	}
	if agent.GiteaUserID == 0 {
		t.Error("expected non-zero Gitea user ID")
	}
}

func TestGetOrCreateAgent_ReusesIdle(t *testing.T) {
	mgr, _, _ := setupTestManager(t)

	// Create first agent
	agent1, _ := mgr.GetOrCreateAgent("backend", map[string]bool{})

	// Request another backend agent — should reuse since agent1 is not busy
	agent2, _ := mgr.GetOrCreateAgent("backend", map[string]bool{})

	if agent1.ID != agent2.ID {
		t.Errorf("expected reuse of %q, got new %q", agent1.ID, agent2.ID)
	}
}

func TestGetOrCreateAgent_CreatesNewWhenAllBusy(t *testing.T) {
	mgr, _, _ := setupTestManager(t)

	agent1, _ := mgr.GetOrCreateAgent("backend", map[string]bool{})

	// Mark agent1 as busy
	busy := map[string]bool{agent1.ID: true}
	agent2, err := mgr.GetOrCreateAgent("backend", busy)
	if err != nil {
		t.Fatalf("GetOrCreateAgent: %v", err)
	}

	if agent1.ID == agent2.ID {
		t.Error("expected a new agent when all are busy")
	}
	if agent2.ID != "backend-agent-2" {
		t.Errorf("expected 'backend-agent-2', got %q", agent2.ID)
	}
}

func TestGetOrCreateAgent_DifferentSpecializations(t *testing.T) {
	mgr, _, _ := setupTestManager(t)

	a1, _ := mgr.GetOrCreateAgent("frontend", map[string]bool{})
	a2, _ := mgr.GetOrCreateAgent("backend", map[string]bool{})

	if a1.ID == a2.ID {
		t.Error("different specializations should produce different agents")
	}
	if a1.Specialization != "frontend" || a2.Specialization != "backend" {
		t.Errorf("wrong specializations: %s, %s", a1.Specialization, a2.Specialization)
	}
}

func TestGetAgent(t *testing.T) {
	mgr, _, _ := setupTestManager(t)

	mgr.GetOrCreateAgent("test", map[string]bool{})

	agent := mgr.GetAgent("test-agent-1")
	if agent == nil {
		t.Fatal("expected to find test-agent-1")
	}

	missing := mgr.GetAgent("nonexistent")
	if missing != nil {
		t.Error("expected nil for nonexistent agent")
	}
}

func TestListAgents(t *testing.T) {
	mgr, _, _ := setupTestManager(t)

	mgr.GetOrCreateAgent("frontend", map[string]bool{})
	mgr.GetOrCreateAgent("backend", map[string]bool{})

	agents := mgr.ListAgents()
	if len(agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(agents))
	}
}

func TestListBySpecialization(t *testing.T) {
	mgr, _, _ := setupTestManager(t)

	mgr.GetOrCreateAgent("frontend", map[string]bool{})
	mgr.GetOrCreateAgent("frontend", map[string]bool{"frontend-agent-1": true})
	mgr.GetOrCreateAgent("backend", map[string]bool{})

	frontendAgents := mgr.ListBySpecialization("frontend")
	if len(frontendAgents) != 2 {
		t.Errorf("expected 2 frontend agents, got %d", len(frontendAgents))
	}

	backendAgents := mgr.ListBySpecialization("backend")
	if len(backendAgents) != 1 {
		t.Errorf("expected 1 backend agent, got %d", len(backendAgents))
	}
}

func TestHydrateFromGitea(t *testing.T) {
	giteaMux := http.NewServeMux()
	giteaMux.HandleFunc("/api/v1/users/search", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []gitea.GiteaUser{
				{ID: 42, Login: "frontend-agent-5"},
				{ID: 7, Login: "general-agent-2"},
				{ID: 1, Login: "claude"}, // not an agent — must be ignored
			},
			"ok": true,
		})
	})
	giteaSrv := httptest.NewServer(giteaMux)
	t.Cleanup(giteaSrv.Close)

	taigaMux := http.NewServeMux()
	taigaMux.HandleFunc("/api/v1/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"auth_token": "test-token"})
	})
	taigaMux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]taiga.User{
			{ID: 99, Username: "frontend-agent-5"},
			{ID: 100, Username: "general-agent-2"},
		})
	})
	taigaSrv := httptest.NewServer(taigaMux)
	t.Cleanup(taigaSrv.Close)

	giteaClient := gitea.NewClient(giteaSrv.URL, "admin", "password")
	taigaClient := taiga.NewClient(taigaSrv.URL)
	taigaClient.Authenticate("admin", "password")
	mgr := NewManager(giteaClient, taigaClient, nil, nil, 1, 1)

	if err := mgr.HydrateFromGitea(); err != nil {
		t.Fatalf("HydrateFromGitea: %v", err)
	}

	if a := mgr.GetAgent("frontend-agent-5"); a == nil || a.GiteaUserID != 42 || a.TaigaUserID != 99 {
		t.Errorf("frontend-agent-5 hydration wrong: %+v", a)
	}
	if a := mgr.GetAgent("general-agent-2"); a == nil || a.GiteaUserID != 7 {
		t.Errorf("general-agent-2 hydration wrong: %+v", a)
	}
	if mgr.GetAgent("claude") != nil {
		t.Errorf("non-agent user should not be hydrated")
	}

	// counts must reflect highest seen number per spec, so new agents start at +1
	if mgr.counts["frontend"] != 5 {
		t.Errorf("frontend count expected 5, got %d", mgr.counts["frontend"])
	}
	if mgr.counts["general"] != 2 {
		t.Errorf("general count expected 2, got %d", mgr.counts["general"])
	}
}

func TestRemoveAgent(t *testing.T) {
	mgr, _, _ := setupTestManager(t)

	mgr.GetOrCreateAgent("test", map[string]bool{})
	if mgr.GetAgent("test-agent-1") == nil {
		t.Fatal("agent should exist before removal")
	}

	mgr.RemoveAgent("test-agent-1")
	if mgr.GetAgent("test-agent-1") != nil {
		t.Error("agent should be gone after removal")
	}
}

func TestDefaultSpecializations(t *testing.T) {
	expected := []string{"general", "frontend", "backend", "test", "documentation", "operations"}
	if len(DefaultSpecializations) != len(expected) {
		t.Errorf("expected %d specializations, got %d", len(expected), len(DefaultSpecializations))
	}
	for i, spec := range expected {
		if DefaultSpecializations[i] != spec {
			t.Errorf("expected specialization[%d]=%q, got %q", i, spec, DefaultSpecializations[i])
		}
	}
	fmt.Println("DefaultSpecializations:", DefaultSpecializations)
}
