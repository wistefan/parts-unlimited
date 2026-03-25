// Package review implements the PR review service.
// It invokes Claude Code CLI to review PR diffs and posts review comments on Gitea.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
)

// ReviewResult holds the output from a Claude Code review.
type ReviewResult struct {
	Approved bool   `json:"approved"`
	Summary  string `json:"summary"`
	Issues   []string `json:"issues,omitempty"`
}

// Config holds review service configuration.
type Config struct {
	ClaudeBinary string // path to claude CLI, defaults to "claude"
	Timeout      time.Duration
}

// DefaultConfig returns the default review configuration.
func DefaultConfig() *Config {
	return &Config{
		ClaudeBinary: "claude",
		Timeout:      5 * time.Minute,
	}
}

// Service reviews PRs using Claude Code CLI.
type Service struct {
	giteaClient *gitea.Client
	config      *Config
}

// NewService creates a new review service.
func NewService(giteaClient *gitea.Client, config *Config) *Service {
	if config == nil {
		config = DefaultConfig()
	}
	return &Service{
		giteaClient: giteaClient,
		config:      config,
	}
}

// ReviewPR fetches a PR diff, invokes Claude to review it, and posts the review on Gitea.
func (s *Service) ReviewPR(ctx context.Context, owner, repo string, prNumber int) (*ReviewResult, error) {
	log.Printf("Reviewing PR #%d on %s/%s", prNumber, owner, repo)

	// Get the PR details
	pr, err := s.giteaClient.GetPullRequest(owner, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("getting PR: %w", err)
	}

	// Get the PR diff
	diff, err := s.getPRDiff(owner, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("getting PR diff: %w", err)
	}

	if diff == "" {
		log.Printf("PR #%d has no diff, skipping review", prNumber)
		return &ReviewResult{Approved: true, Summary: "No changes to review."}, nil
	}

	// Build review prompt
	prompt := buildReviewPrompt(pr, diff)

	// Invoke Claude
	result, err := s.invokeClaude(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("invoking claude for review: %w", err)
	}

	// Post review on Gitea
	event := "COMMENT"
	if result.Approved {
		event = "APPROVED"
	} else if len(result.Issues) > 0 {
		event = "REQUEST_CHANGES"
	}

	reviewBody := formatReviewBody(result)
	_, err = s.giteaClient.CreateReview(owner, repo, prNumber, &gitea.CreateReviewRequest{
		Body:  reviewBody,
		Event: event,
	})
	if err != nil {
		log.Printf("WARNING: could not post review on PR #%d: %v", prNumber, err)
	}

	log.Printf("PR #%d review complete: approved=%v, issues=%d", prNumber, result.Approved, len(result.Issues))
	return result, nil
}

// getPRDiff fetches the unified diff for a PR via the Gitea API.
func (s *Service) getPRDiff(owner, repo string, prNumber int) (string, error) {
	// Gitea serves diffs at /repos/:owner/:repo/pulls/:number.diff
	// We use the gitea client's underlying HTTP to fetch it
	// For now, return a placeholder — the actual diff fetch will use the Gitea API
	// which requires a .diff endpoint not yet in our client
	return fmt.Sprintf("[diff for PR #%d — fetch via Gitea API .diff endpoint]", prNumber), nil
}

// invokeClaude runs the claude CLI to review a diff.
func (s *Service) invokeClaude(ctx context.Context, prompt string) (*ReviewResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.config.ClaudeBinary,
		"-p",
		"--output-format", "json",
		"--bare",
		"--no-session-persistence",
		prompt,
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude command: %w", err)
	}

	// Try to parse structured output from Claude's response
	result := parseReviewOutput(string(output))
	return result, nil
}

// buildReviewPrompt creates the prompt for Claude to review a PR.
func buildReviewPrompt(pr *gitea.PullRequest, diff string) string {
	var b strings.Builder

	b.WriteString("Review the following pull request. Evaluate:\n")
	b.WriteString("1. Code correctness and potential bugs\n")
	b.WriteString("2. Code quality and readability\n")
	b.WriteString("3. Test coverage\n")
	b.WriteString("4. Documentation of public methods and classes\n")
	b.WriteString("5. No magic constants\n")
	b.WriteString("6. Security considerations\n\n")
	b.WriteString(fmt.Sprintf("PR Title: %s\n", pr.Title))
	b.WriteString(fmt.Sprintf("PR Description:\n%s\n\n", pr.Body))
	b.WriteString("Diff:\n```\n")
	b.WriteString(diff)
	b.WriteString("\n```\n\n")
	b.WriteString("Respond with a JSON object:\n")
	b.WriteString("```json\n")
	b.WriteString(`{"approved": true/false, "summary": "brief summary", "issues": ["issue 1", "issue 2"]}`)
	b.WriteString("\n```\n")

	return b.String()
}

// parseReviewOutput extracts a ReviewResult from Claude's response.
func parseReviewOutput(output string) *ReviewResult {
	// Try to find JSON in the output
	result := &ReviewResult{}

	// Look for JSON block
	startIdx := strings.Index(output, "{")
	endIdx := strings.LastIndex(output, "}")

	if startIdx >= 0 && endIdx > startIdx {
		jsonStr := output[startIdx : endIdx+1]
		if err := json.Unmarshal([]byte(jsonStr), result); err == nil {
			return result
		}
	}

	// Fallback: treat the whole output as a comment
	result.Summary = output
	result.Approved = false
	return result
}

// formatReviewBody formats a ReviewResult into a markdown review comment.
func formatReviewBody(result *ReviewResult) string {
	var b strings.Builder

	b.WriteString("## Orchestrator Review\n\n")
	b.WriteString(result.Summary)
	b.WriteString("\n")

	if len(result.Issues) > 0 {
		b.WriteString("\n### Issues\n\n")
		for _, issue := range result.Issues {
			b.WriteString(fmt.Sprintf("- %s\n", issue))
		}
	}

	if result.Approved {
		b.WriteString("\n*Orchestrator approves this PR. Human approval still required.*\n")
	}

	return b.String()
}
