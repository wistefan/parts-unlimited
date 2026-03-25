// Package plan implements the implementation plan workflow:
// parsing plans, tracking step execution, and managing the plan lifecycle.
package plan

import (
	"fmt"
	"regexp"
	"strings"
)

// StepStatus represents the execution state of a plan step.
type StepStatus string

const (
	StepPending    StepStatus = "pending"
	StepInProgress StepStatus = "in_progress"
	StepCompleted  StepStatus = "completed"
	StepBlocked    StepStatus = "blocked"
)

// PlanPhase represents the current phase of the plan lifecycle.
type PlanPhase string

const (
	PhasePlanCreation    PlanPhase = "plan_creation"
	PhaseAwaitingApproval PlanPhase = "awaiting_approval"
	PhaseExecuting       PlanPhase = "executing"
	PhaseRevisionPending PlanPhase = "revision_pending"
	PhaseCompleted       PlanPhase = "completed"
)

// Step represents a single step in an implementation plan.
type Step struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	Specialization string     `json:"specialization,omitempty"` // empty = general
	Parallel       bool       `json:"parallel"`                 // can run in parallel with next step
	Status         StepStatus `json:"status"`
	AgentID        string     `json:"agentId,omitempty"`
	PRNumber       int        `json:"prNumber,omitempty"`
}

// Plan represents a parsed implementation plan for a ticket.
type Plan struct {
	TicketID  int       `json:"ticketId"`
	RepoOwner string   `json:"repoOwner"`
	RepoName  string   `json:"repoName"`
	Phase     PlanPhase `json:"phase"`
	Steps     []Step    `json:"steps"`
	PRID      int       `json:"prId,omitempty"` // PR containing the plan itself
}

// PlanBranchName returns the branch name for the plan PR.
func PlanBranchName(agentID string, ticketID int) string {
	return fmt.Sprintf("agent/%s/ticket-%d/plan", agentID, ticketID)
}

// StepBranchName returns the branch name for a step PR.
func StepBranchName(agentID string, ticketID, stepNumber int) string {
	return fmt.Sprintf("agent/%s/ticket-%d/step-%d", agentID, ticketID, stepNumber)
}

// NextPendingSteps returns the next steps that are ready to execute.
// If the next pending step is parallel, returns all consecutive parallel steps.
// Otherwise returns a single step.
func (p *Plan) NextPendingSteps() []Step {
	var result []Step

	for i, step := range p.Steps {
		if step.Status != StepPending {
			continue
		}

		result = append(result, step)

		// If this step is marked parallel, collect subsequent parallel steps
		if step.Parallel {
			for j := i + 1; j < len(p.Steps); j++ {
				if p.Steps[j].Status != StepPending {
					break
				}
				result = append(result, p.Steps[j])
				if !p.Steps[j].Parallel {
					break
				}
			}
		}
		break
	}

	return result
}

// AllStepsCompleted returns true if every step in the plan is completed.
func (p *Plan) AllStepsCompleted() bool {
	for _, step := range p.Steps {
		if step.Status != StepCompleted {
			return false
		}
	}
	return len(p.Steps) > 0
}

// CompletedCount returns how many steps are completed.
func (p *Plan) CompletedCount() int {
	count := 0
	for _, step := range p.Steps {
		if step.Status == StepCompleted {
			count++
		}
	}
	return count
}

// SetStepStatus updates the status of a step by number.
func (p *Plan) SetStepStatus(stepNumber int, status StepStatus) bool {
	for i, step := range p.Steps {
		if step.Number == stepNumber {
			p.Steps[i].Status = status
			return true
		}
	}
	return false
}

// SetStepAgent assigns an agent to a step.
func (p *Plan) SetStepAgent(stepNumber int, agentID string) bool {
	for i, step := range p.Steps {
		if step.Number == stepNumber {
			p.Steps[i].AgentID = agentID
			return true
		}
	}
	return false
}

// SetStepPR records the PR number for a step.
func (p *Plan) SetStepPR(stepNumber, prNumber int) bool {
	for i, step := range p.Steps {
		if step.Number == stepNumber {
			p.Steps[i].PRNumber = prNumber
			return true
		}
	}
	return false
}

// stepHeaderRegex matches markdown headers like "### Step 1: Title" or "## 1. Title"
var stepHeaderRegex = regexp.MustCompile(
	`(?m)^#{2,4}\s+(?:Step\s+)?(\d+)[.:\s-]+\s*(.+)$`,
)

// parallelMarkerRegex detects parallelism markers in step descriptions.
var parallelMarkerRegex = regexp.MustCompile(
	`(?i)(?:can be parallelized|parallel(?:izable)?|concurrent)`,
)

// specializationRegex detects specialization requirements in step descriptions.
var specializationRegex = regexp.MustCompile(
	`(?i)(?:specialization|role|requires?):\s*(frontend|backend|test|documentation|operations)`,
)

// ParsePlan extracts steps from a markdown implementation plan.
func ParsePlan(content string, ticketID int, repoOwner, repoName string) *Plan {
	plan := &Plan{
		TicketID:  ticketID,
		RepoOwner: repoOwner,
		RepoName:  repoName,
		Phase:     PhaseAwaitingApproval,
	}

	matches := stepHeaderRegex.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return plan
	}

	for i, match := range matches {
		numStr := content[match[2]:match[3]]
		title := strings.TrimSpace(content[match[4]:match[5]])

		var num int
		fmt.Sscanf(numStr, "%d", &num)

		// Extract the body between this header and the next (or end)
		bodyStart := match[1]
		bodyEnd := len(content)
		if i+1 < len(matches) {
			bodyEnd = matches[i+1][0]
		}
		body := content[bodyStart:bodyEnd]

		step := Step{
			Number:      num,
			Title:       title,
			Description: strings.TrimSpace(body),
			Status:      StepPending,
			Parallel:    parallelMarkerRegex.MatchString(body),
		}

		// Detect specialization
		if specMatch := specializationRegex.FindStringSubmatch(body); len(specMatch) > 1 {
			step.Specialization = strings.ToLower(specMatch[1])
		}

		plan.Steps = append(plan.Steps, step)
	}

	return plan
}

// FormatReleaseNotes generates release notes from a completed plan.
func FormatReleaseNotes(plan *Plan, ticketSubject string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("# Release Notes: %s\n\n", ticketSubject))
	b.WriteString(fmt.Sprintf("**Ticket:** #%d\n", plan.TicketID))
	b.WriteString(fmt.Sprintf("**Repository:** %s/%s\n\n", plan.RepoOwner, plan.RepoName))
	b.WriteString("## Changes\n\n")

	for _, step := range plan.Steps {
		status := "completed"
		if step.Status != StepCompleted {
			status = string(step.Status)
		}
		prRef := ""
		if step.PRNumber > 0 {
			prRef = fmt.Sprintf(" (PR #%d)", step.PRNumber)
		}
		b.WriteString(fmt.Sprintf("- **Step %d:** %s — %s%s\n", step.Number, step.Title, status, prRef))
	}

	return b.String()
}
