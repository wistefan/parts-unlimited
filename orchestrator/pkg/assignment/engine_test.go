package assignment

import (
	"testing"
)

func TestEnqueueAndDequeue(t *testing.T) {
	e := NewEngine(3, 2)

	e.Enqueue(1)
	e.Enqueue(2)
	e.Enqueue(3)

	if e.QueueLength() != 3 {
		t.Errorf("expected queue length 3, got %d", e.QueueLength())
	}

	// Dequeue in FIFO order
	entry := e.Dequeue()
	if entry == nil || entry.TicketID != 1 {
		t.Errorf("expected ticket 1, got %v", entry)
	}

	entry = e.Dequeue()
	if entry == nil || entry.TicketID != 2 {
		t.Errorf("expected ticket 2, got %v", entry)
	}

	if e.QueueLength() != 1 {
		t.Errorf("expected queue length 1, got %d", e.QueueLength())
	}
}

func TestEnqueue_NoDuplicates(t *testing.T) {
	e := NewEngine(3, 2)

	e.Enqueue(42)
	e.Enqueue(42)

	if e.QueueLength() != 1 {
		t.Errorf("expected queue length 1 (no duplicates), got %d", e.QueueLength())
	}
}

func TestEnqueue_SkipsAssigned(t *testing.T) {
	e := NewEngine(3, 2)

	e.Enqueue(10)
	entry := e.Dequeue()
	e.AssignAgent(entry.TicketID, "agent-1")

	// Try to enqueue again — should be skipped since it's assigned
	e.Enqueue(10)
	if e.QueueLength() != 0 {
		t.Errorf("expected empty queue (ticket already assigned), got %d", e.QueueLength())
	}
}

func TestDequeue_RespectsMaxConcurrency(t *testing.T) {
	e := NewEngine(2, 2)

	e.Enqueue(1)
	e.Enqueue(2)
	e.Enqueue(3)

	// Assign two agents — max concurrency reached
	entry1 := e.Dequeue()
	e.AssignAgent(entry1.TicketID, "agent-1")
	entry2 := e.Dequeue()
	e.AssignAgent(entry2.TicketID, "agent-2")

	// Third dequeue should return nil
	entry3 := e.Dequeue()
	if entry3 != nil {
		t.Errorf("expected nil at max concurrency, got ticket %d", entry3.TicketID)
	}

	if e.ActiveCount() != 2 {
		t.Errorf("expected 2 active, got %d", e.ActiveCount())
	}
}

func TestDequeue_EmptyQueue(t *testing.T) {
	e := NewEngine(3, 2)

	entry := e.Dequeue()
	if entry != nil {
		t.Error("expected nil from empty queue")
	}
}

func TestAssignAgent(t *testing.T) {
	e := NewEngine(3, 2)

	e.AssignAgent(42, "general-agent-1")

	assignment := e.GetAssignment(42)
	if assignment == nil {
		t.Fatal("expected assignment for ticket 42")
	}
	if assignment.PrimaryAgent != "general-agent-1" {
		t.Errorf("expected primary agent 'general-agent-1', got %q", assignment.PrimaryAgent)
	}
	if assignment.Status != "assigned" {
		t.Errorf("expected status 'assigned', got %q", assignment.Status)
	}

	busy := e.GetBusyAgents()
	if !busy["general-agent-1"] {
		t.Error("expected general-agent-1 to be busy")
	}
}

func TestDelegation(t *testing.T) {
	e := NewEngine(5, 2)

	// Primary assignment
	e.AssignAgent(42, "general-agent-1")

	// Delegate to frontend and test
	e.RecordDelegation(42, "frontend-agent-1")
	e.RecordDelegation(42, "test-agent-1")

	assignment := e.GetAssignment(42)
	if len(assignment.DelegatedTo) != 2 {
		t.Errorf("expected 2 delegations, got %d", len(assignment.DelegatedTo))
	}
	if assignment.Status != "delegated" {
		t.Errorf("expected status 'delegated', got %q", assignment.Status)
	}

	// Complete frontend delegation
	allDone := e.CompleteDelegation(42, "frontend-agent-1")
	if allDone {
		t.Error("should not be all done — test-agent-1 still working")
	}

	// Complete test delegation
	allDone = e.CompleteDelegation(42, "test-agent-1")
	if !allDone {
		t.Error("expected all delegations complete")
	}

	assignment = e.GetAssignment(42)
	if assignment.Status != "assigned" {
		t.Errorf("expected status 'assigned' after all delegations complete, got %q", assignment.Status)
	}
}

func TestCompleteTicket(t *testing.T) {
	e := NewEngine(3, 2)

	e.AssignAgent(42, "general-agent-1")
	e.RecordDelegation(42, "frontend-agent-1")

	e.CompleteTicket(42)

	if e.GetAssignment(42) != nil {
		t.Error("expected no assignment after completion")
	}
	if e.ActiveCount() != 0 {
		t.Errorf("expected 0 active agents, got %d", e.ActiveCount())
	}
}

func TestEscalation(t *testing.T) {
	e := NewEngine(3, 2) // threshold = 2

	escalate := e.RecordReassignment(42)
	if escalate {
		t.Error("should not escalate after 1 reassignment")
	}

	escalate = e.RecordReassignment(42)
	if !escalate {
		t.Error("should escalate after 2 reassignments")
	}
}

func TestEscalation_Reset(t *testing.T) {
	e := NewEngine(3, 2)

	e.RecordReassignment(42)
	e.ResetEscalation(42)

	// After reset, should take 2 more reassignments to escalate
	escalate := e.RecordReassignment(42)
	if escalate {
		t.Error("should not escalate after reset + 1 reassignment")
	}
}

func TestDelegateToSpecialization(t *testing.T) {
	e := NewEngine(3, 2)

	tests := []struct {
		tag    string
		expect string
	}{
		{"delegate:frontend", "frontend"},
		{"delegate:test", "test"},
		{"delegate:backend", "backend"},
		{"active:frontend", ""},
		{"other-tag", ""},
		{"delegate:", ""},
	}

	for _, tt := range tests {
		result := e.DelegateToSpecialization(tt.tag)
		if result != tt.expect {
			t.Errorf("DelegateToSpecialization(%q): expected %q, got %q", tt.tag, tt.expect, result)
		}
	}
}

func TestExtractDelegationTags(t *testing.T) {
	tags := []string{"frontend", "delegate:test", "delegate:documentation", "active:backend"}
	result := ExtractDelegationTags(tags)

	if len(result) != 2 {
		t.Errorf("expected 2 delegation tags, got %d", len(result))
	}
}

func TestExtractActiveTags(t *testing.T) {
	tags := []string{"frontend", "delegate:test", "active:backend", "active:frontend"}
	result := ExtractActiveTags(tags)

	if len(result) != 2 {
		t.Errorf("expected 2 active tags, got %d", len(result))
	}
}

func TestReplaceDelegateWithActive(t *testing.T) {
	tags := []string{"frontend", "delegate:test", "other"}
	result := ReplaceDelegateWithActive(tags, "test")

	expected := []string{"frontend", "active:test", "other"}
	if len(result) != len(expected) {
		t.Fatalf("expected %d tags, got %d", len(expected), len(result))
	}
	for i, tag := range result {
		if tag != expected[i] {
			t.Errorf("tag[%d]: expected %q, got %q", i, expected[i], tag)
		}
	}
}

func TestRemoveActiveTag(t *testing.T) {
	tags := []string{"frontend", "active:test", "active:backend", "other"}
	result := RemoveActiveTag(tags, "test")

	if len(result) != 3 {
		t.Errorf("expected 3 tags after removal, got %d", len(result))
	}
	for _, tag := range result {
		if tag == "active:test" {
			t.Error("active:test should have been removed")
		}
	}
}

func TestHasActiveDelegations(t *testing.T) {
	if HasActiveDelegations([]string{"frontend", "delegate:test"}) {
		t.Error("should not have active delegations")
	}
	if !HasActiveDelegations([]string{"frontend", "active:backend"}) {
		t.Error("should have active delegations")
	}
	if HasActiveDelegations([]string{}) {
		t.Error("empty tags should not have active delegations")
	}
}

func TestFormatEscalationComment(t *testing.T) {
	comment := FormatEscalationComment(42, "agents disagreed on approach")
	if comment == "" {
		t.Error("expected non-empty comment")
	}
	if !containsString(comment, "Escalation") {
		t.Error("expected comment to contain 'Escalation'")
	}
	if !containsString(comment, "agents disagreed on approach") {
		t.Error("expected comment to contain the reason")
	}
}

func TestGetQueue(t *testing.T) {
	e := NewEngine(3, 2)

	e.Enqueue(10)
	e.Enqueue(20)

	queue := e.GetQueue()
	if len(queue) != 2 {
		t.Errorf("expected 2 entries, got %d", len(queue))
	}
	if queue[0].TicketID != 10 {
		t.Errorf("expected first entry ticket 10, got %d", queue[0].TicketID)
	}
}

func TestGetAllAssignments(t *testing.T) {
	e := NewEngine(3, 2)

	e.AssignAgent(1, "agent-a")
	e.AssignAgent(2, "agent-b")

	all := e.GetAllAssignments()
	if len(all) != 2 {
		t.Errorf("expected 2 assignments, got %d", len(all))
	}
}

func containsString(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 && contains(haystack, needle)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
