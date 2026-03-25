package state

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wistefan/dev-env/orchestrator/pkg/assignment"
	"github.com/wistefan/dev-env/orchestrator/pkg/identity"
	"github.com/wistefan/dev-env/orchestrator/pkg/plan"
)

func newTestManager() (*Manager, *fake.Clientset) {
	clientset := fake.NewSimpleClientset()
	mgr := NewManager(clientset, "agents")

	// Create namespace
	clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "agents"},
	}, metav1.CreateOptions{})

	return mgr, clientset
}

func sampleState() *OrchestratorState {
	return &OrchestratorState{
		Agents: []identity.AgentIdentity{
			{ID: "general-agent-1", Specialization: "general", GiteaUserID: 5, TaigaUserID: 10},
		},
		Queue: []assignment.QueueEntry{
			{TicketID: 55},
		},
		Assignments: map[int]*assignment.TicketAssignment{
			42: {TicketID: 42, PrimaryAgent: "general-agent-1", Status: "assigned"},
		},
		Escalations: map[int]*assignment.EscalationEntry{},
		Plans: map[int]*plan.Plan{
			42: {TicketID: 42, Phase: plan.PhaseExecuting, Steps: []plan.Step{
				{Number: 1, Title: "Setup", Status: plan.StepCompleted},
				{Number: 2, Title: "Build", Status: plan.StepInProgress},
			}},
		},
	}
}

func TestSaveAndLoad(t *testing.T) {
	mgr, _ := newTestManager()
	ctx := context.Background()

	state := sampleState()

	if err := mgr.Save(ctx, state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil loaded state")
	}

	if len(loaded.Agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(loaded.Agents))
	}
	if loaded.Agents[0].ID != "general-agent-1" {
		t.Errorf("expected agent ID 'general-agent-1', got %q", loaded.Agents[0].ID)
	}
	if len(loaded.Queue) != 1 {
		t.Errorf("expected 1 queue entry, got %d", len(loaded.Queue))
	}
	if loaded.Queue[0].TicketID != 55 {
		t.Errorf("expected ticket 55 in queue, got %d", loaded.Queue[0].TicketID)
	}
	if loaded.Assignments[42] == nil {
		t.Fatal("expected assignment for ticket 42")
	}
	if loaded.Assignments[42].PrimaryAgent != "general-agent-1" {
		t.Errorf("expected primary agent, got %q", loaded.Assignments[42].PrimaryAgent)
	}
	if loaded.Plans[42] == nil {
		t.Fatal("expected plan for ticket 42")
	}
	if loaded.Plans[42].Steps[1].Status != plan.StepInProgress {
		t.Errorf("expected step 2 in progress, got %q", loaded.Plans[42].Steps[1].Status)
	}
	if loaded.LastSaved.IsZero() {
		t.Error("expected non-zero LastSaved")
	}
}

func TestLoad_NoExistingState(t *testing.T) {
	mgr, _ := newTestManager()
	ctx := context.Background()

	loaded, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil state when ConfigMap doesn't exist")
	}
}

func TestSave_UpdateExisting(t *testing.T) {
	mgr, _ := newTestManager()
	ctx := context.Background()

	state1 := sampleState()
	if err := mgr.Save(ctx, state1); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	// Modify and save again
	state2 := sampleState()
	state2.Queue = append(state2.Queue, assignment.QueueEntry{TicketID: 99})

	if err := mgr.Save(ctx, state2); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	loaded, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Queue) != 2 {
		t.Errorf("expected 2 queue entries after update, got %d", len(loaded.Queue))
	}
}

func TestDelete(t *testing.T) {
	mgr, _ := newTestManager()
	ctx := context.Background()

	state := sampleState()
	mgr.Save(ctx, state)

	if err := mgr.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	loaded, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil state after delete")
	}
}

func TestDelete_NotFound(t *testing.T) {
	mgr, _ := newTestManager()
	ctx := context.Background()

	// Deleting non-existent state should not error
	if err := mgr.Delete(ctx); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestStateJSON_RoundTrip(t *testing.T) {
	state := sampleState()

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var loaded OrchestratorState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(loaded.Agents) != len(state.Agents) {
		t.Errorf("agents mismatch: %d vs %d", len(loaded.Agents), len(state.Agents))
	}
	if len(loaded.Plans) != len(state.Plans) {
		t.Errorf("plans mismatch: %d vs %d", len(loaded.Plans), len(state.Plans))
	}
}
