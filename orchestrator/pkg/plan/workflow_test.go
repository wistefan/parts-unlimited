package plan

import (
	"strings"
	"testing"
)

const samplePlan = `# Implementation Plan

## Step 1: Setup database schema

Create the initial database tables.

## Step 2: Implement API endpoints

Build the REST API. Can be parallelized with step 3.

## Step 3: Write frontend components

Build the UI. Specialization: frontend. Can be parallelized.

## Step 4: Integration tests

End-to-end tests. Specialization: test
`

func TestParsePlan(t *testing.T) {
	plan := ParsePlan(samplePlan, 42, "claude", "myrepo")

	if plan.TicketID != 42 {
		t.Errorf("expected ticketID=42, got %d", plan.TicketID)
	}
	if len(plan.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(plan.Steps))
	}

	// Step 1
	if plan.Steps[0].Number != 1 {
		t.Errorf("step 1 number: got %d", plan.Steps[0].Number)
	}
	if plan.Steps[0].Title != "Setup database schema" {
		t.Errorf("step 1 title: got %q", plan.Steps[0].Title)
	}
	if plan.Steps[0].Parallel {
		t.Error("step 1 should not be parallel")
	}
	if plan.Steps[0].Status != StepPending {
		t.Errorf("step 1 status: got %q", plan.Steps[0].Status)
	}

	// Step 2 — parallel
	if !plan.Steps[1].Parallel {
		t.Error("step 2 should be parallel")
	}

	// Step 3 — parallel + frontend specialization
	if !plan.Steps[2].Parallel {
		t.Error("step 3 should be parallel")
	}
	if plan.Steps[2].Specialization != "frontend" {
		t.Errorf("step 3 specialization: expected 'frontend', got %q", plan.Steps[2].Specialization)
	}

	// Step 4 — test specialization
	if plan.Steps[3].Specialization != "test" {
		t.Errorf("step 4 specialization: expected 'test', got %q", plan.Steps[3].Specialization)
	}
}

func TestParsePlan_Empty(t *testing.T) {
	plan := ParsePlan("No steps here, just text.", 1, "o", "r")
	if len(plan.Steps) != 0 {
		t.Errorf("expected 0 steps for planless content, got %d", len(plan.Steps))
	}
}

func TestNextPendingSteps_Sequential(t *testing.T) {
	plan := &Plan{
		Steps: []Step{
			{Number: 1, Status: StepCompleted},
			{Number: 2, Status: StepPending, Parallel: false},
			{Number: 3, Status: StepPending},
		},
	}

	next := plan.NextPendingSteps()
	if len(next) != 1 {
		t.Fatalf("expected 1 step, got %d", len(next))
	}
	if next[0].Number != 2 {
		t.Errorf("expected step 2, got %d", next[0].Number)
	}
}

func TestNextPendingSteps_Parallel(t *testing.T) {
	plan := &Plan{
		Steps: []Step{
			{Number: 1, Status: StepCompleted},
			{Number: 2, Status: StepPending, Parallel: true},
			{Number: 3, Status: StepPending, Parallel: true},
			{Number: 4, Status: StepPending, Parallel: false},
		},
	}

	next := plan.NextPendingSteps()
	if len(next) != 3 {
		t.Fatalf("expected 3 parallel steps, got %d", len(next))
	}
}

func TestNextPendingSteps_AllCompleted(t *testing.T) {
	plan := &Plan{
		Steps: []Step{
			{Number: 1, Status: StepCompleted},
			{Number: 2, Status: StepCompleted},
		},
	}

	next := plan.NextPendingSteps()
	if len(next) != 0 {
		t.Errorf("expected 0 steps when all completed, got %d", len(next))
	}
}

func TestAllStepsCompleted(t *testing.T) {
	plan := &Plan{
		Steps: []Step{
			{Number: 1, Status: StepCompleted},
			{Number: 2, Status: StepCompleted},
		},
	}
	if !plan.AllStepsCompleted() {
		t.Error("expected all steps completed")
	}

	plan.Steps[1].Status = StepInProgress
	if plan.AllStepsCompleted() {
		t.Error("expected not all completed")
	}
}

func TestAllStepsCompleted_Empty(t *testing.T) {
	plan := &Plan{}
	if plan.AllStepsCompleted() {
		t.Error("empty plan should not be completed")
	}
}

func TestCompletedCount(t *testing.T) {
	plan := &Plan{
		Steps: []Step{
			{Number: 1, Status: StepCompleted},
			{Number: 2, Status: StepInProgress},
			{Number: 3, Status: StepCompleted},
		},
	}
	if plan.CompletedCount() != 2 {
		t.Errorf("expected 2 completed, got %d", plan.CompletedCount())
	}
}

func TestSetStepStatus(t *testing.T) {
	plan := &Plan{
		Steps: []Step{{Number: 1, Status: StepPending}},
	}

	if !plan.SetStepStatus(1, StepInProgress) {
		t.Error("expected SetStepStatus to return true")
	}
	if plan.Steps[0].Status != StepInProgress {
		t.Errorf("expected in_progress, got %q", plan.Steps[0].Status)
	}

	if plan.SetStepStatus(99, StepCompleted) {
		t.Error("expected false for non-existent step")
	}
}

func TestSetStepAgent(t *testing.T) {
	plan := &Plan{
		Steps: []Step{{Number: 1}},
	}

	plan.SetStepAgent(1, "frontend-agent-1")
	if plan.Steps[0].AgentID != "frontend-agent-1" {
		t.Errorf("expected agent set, got %q", plan.Steps[0].AgentID)
	}
}

func TestSetStepPR(t *testing.T) {
	plan := &Plan{
		Steps: []Step{{Number: 1}},
	}

	plan.SetStepPR(1, 5)
	if plan.Steps[0].PRNumber != 5 {
		t.Errorf("expected PR 5, got %d", plan.Steps[0].PRNumber)
	}
}

func TestPlanBranchName(t *testing.T) {
	name := PlanBranchName("general-agent-1", 42)
	if name != "agent/general-agent-1/ticket-42/plan" {
		t.Errorf("unexpected branch name: %q", name)
	}
}

func TestStepBranchName(t *testing.T) {
	name := StepBranchName("frontend-agent-1", 42, 3)
	if name != "agent/frontend-agent-1/ticket-42/step-3" {
		t.Errorf("unexpected branch name: %q", name)
	}
}

func TestFormatReleaseNotes(t *testing.T) {
	plan := &Plan{
		TicketID:  42,
		RepoOwner: "claude",
		RepoName:  "myrepo",
		Steps: []Step{
			{Number: 1, Title: "Setup DB", Status: StepCompleted, PRNumber: 2},
			{Number: 2, Title: "Build API", Status: StepCompleted, PRNumber: 3},
		},
	}

	notes := FormatReleaseNotes(plan, "Build user service")
	if !strings.Contains(notes, "Build user service") {
		t.Error("expected ticket subject in notes")
	}
	if !strings.Contains(notes, "#42") {
		t.Error("expected ticket ID in notes")
	}
	if !strings.Contains(notes, "PR #2") {
		t.Error("expected PR reference in notes")
	}
	if !strings.Contains(notes, "Setup DB") {
		t.Error("expected step title in notes")
	}
}

func TestPhaseConstants(t *testing.T) {
	phases := []PlanPhase{PhasePlanCreation, PhaseAwaitingApproval, PhaseExecuting, PhaseRevisionPending, PhaseCompleted}
	for _, p := range phases {
		if p == "" {
			t.Error("empty phase constant")
		}
	}
}
