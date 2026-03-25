package review

import (
	"strings"
	"testing"

	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
)

func TestBuildReviewPrompt(t *testing.T) {
	pr := &gitea.PullRequest{
		Title: "Fix login bug",
		Body:  "Fixes the authentication flow for OAuth users.",
	}

	prompt := buildReviewPrompt(pr, "diff --git a/main.go b/main.go\n+fixed code")

	if !strings.Contains(prompt, "Fix login bug") {
		t.Error("expected PR title in prompt")
	}
	if !strings.Contains(prompt, "authentication flow") {
		t.Error("expected PR body in prompt")
	}
	if !strings.Contains(prompt, "fixed code") {
		t.Error("expected diff in prompt")
	}
	if !strings.Contains(prompt, "magic constants") {
		t.Error("expected quality criteria in prompt")
	}
	if !strings.Contains(prompt, `"approved"`) {
		t.Error("expected JSON format instruction in prompt")
	}
}

func TestParseReviewOutput_ValidJSON(t *testing.T) {
	output := `Here is my review:
{"approved": true, "summary": "Code looks good", "issues": []}
That's all.`

	result := parseReviewOutput(output)
	if !result.Approved {
		t.Error("expected approved=true")
	}
	if result.Summary != "Code looks good" {
		t.Errorf("expected summary 'Code looks good', got %q", result.Summary)
	}
	if len(result.Issues) != 0 {
		t.Errorf("expected 0 issues, got %d", len(result.Issues))
	}
}

func TestParseReviewOutput_WithIssues(t *testing.T) {
	output := `{"approved": false, "summary": "Needs work", "issues": ["Missing tests", "No docs"]}`

	result := parseReviewOutput(output)
	if result.Approved {
		t.Error("expected approved=false")
	}
	if len(result.Issues) != 2 {
		t.Errorf("expected 2 issues, got %d", len(result.Issues))
	}
}

func TestParseReviewOutput_NoJSON(t *testing.T) {
	output := "This is just plain text review without any JSON."

	result := parseReviewOutput(output)
	if result.Approved {
		t.Error("expected not approved for non-JSON output")
	}
	if result.Summary != output {
		t.Error("expected raw output as summary for non-JSON")
	}
}

func TestFormatReviewBody_Approved(t *testing.T) {
	result := &ReviewResult{
		Approved: true,
		Summary:  "Everything looks great.",
	}

	body := formatReviewBody(result)
	if !strings.Contains(body, "Orchestrator Review") {
		t.Error("expected header")
	}
	if !strings.Contains(body, "Everything looks great") {
		t.Error("expected summary")
	}
	if !strings.Contains(body, "Human approval still required") {
		t.Error("expected human approval note")
	}
}

func TestFormatReviewBody_WithIssues(t *testing.T) {
	result := &ReviewResult{
		Approved: false,
		Summary:  "Needs fixes.",
		Issues:   []string{"Missing error handling", "No tests"},
	}

	body := formatReviewBody(result)
	if !strings.Contains(body, "### Issues") {
		t.Error("expected issues section")
	}
	if !strings.Contains(body, "Missing error handling") {
		t.Error("expected issue content")
	}
	if strings.Contains(body, "approves this PR") {
		t.Error("should not show approval note when not approved")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ClaudeBinary != "claude" {
		t.Errorf("expected binary 'claude', got %q", cfg.ClaudeBinary)
	}
	if cfg.Timeout == 0 {
		t.Error("expected non-zero timeout")
	}
}
