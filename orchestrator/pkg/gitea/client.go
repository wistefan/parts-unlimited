// Package gitea provides a client for the Gitea REST API.
package gitea

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a Gitea REST API client using basic authentication.
type Client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

// NewClient creates a new Gitea API client with basic auth credentials.
func NewClient(baseURL, username, password string) *Client {
	return &Client{
		baseURL:  baseURL,
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// --- Repositories ---

// Repository represents a Gitea repository.
type Repository struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Empty         bool   `json:"empty"`
}

// CreateRepo creates a new repository for the authenticated user.
func (c *Client) CreateRepo(name, description string, private bool) (*Repository, error) {
	payload := map[string]interface{}{
		"name":           name,
		"description":    description,
		"private":        private,
		"auto_init":      true,
		"default_branch": "main",
	}

	resp, err := c.doRequest(http.MethodPost, "/api/v1/user/repos", payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create repo: status %d, body: %s", resp.StatusCode, body)
	}

	var repo Repository
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return nil, fmt.Errorf("decoding repo: %w", err)
	}

	return &repo, nil
}

// GetRepo retrieves a repository by owner and name.
func (c *Client) GetRepo(owner, name string) (*Repository, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s", owner, name)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get repo: status %d", resp.StatusCode)
	}

	var repo Repository
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return nil, fmt.Errorf("decoding repo: %w", err)
	}

	return &repo, nil
}

// --- Pull Requests ---

// PullRequest represents a Gitea pull request.
type PullRequest struct {
	ID        int        `json:"id"`
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	HTMLURL   string     `json:"html_url"`
	Head      PRRef      `json:"head"`
	Base      PRRef      `json:"base"`
	Merged    bool       `json:"merged"`
	Mergeable bool       `json:"mergeable"`
	User      *GiteaUser `json:"user,omitempty"`
	Assignees []GiteaUser `json:"assignees,omitempty"`
}

// PRRef represents a branch reference in a pull request.
type PRRef struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
}

// CreatePullRequest creates a new pull request.
func (c *Client) CreatePullRequest(owner, repo string, pr *CreatePRRequest) (*PullRequest, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls", owner, repo)

	resp, err := c.doRequest(http.MethodPost, path, pr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create PR: status %d, body: %s", resp.StatusCode, body)
	}

	var result PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding PR: %w", err)
	}

	return &result, nil
}

// CreatePRRequest holds parameters for creating a pull request.
type CreatePRRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Head      string   `json:"head"`
	Base      string   `json:"base"`
	Assignees []string `json:"assignees,omitempty"`
}

// ListPullRequests lists pull requests for a repository.
func (c *Client) ListPullRequests(owner, repo, state string) ([]PullRequest, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=%s", owner, repo, state)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list PRs: status %d", resp.StatusCode)
	}

	var prs []PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&prs); err != nil {
		return nil, fmt.Errorf("decoding PRs: %w", err)
	}

	return prs, nil
}

// ticketBranchPrefixFmt is the head-branch prefix every orchestrator-managed
// PR carries, regardless of mode (plan / step-N / fix). ListPullRequestsForTicket
// filters on this prefix as a cheap, drift-proof way to associate PRs with
// tickets without a separately maintained mapping.
const ticketBranchPrefixFmt = "ticket-%d/"

// ListPullRequestsForTicket returns every pull request on the repository
// whose head branch is prefixed with `ticket-{ticketID}/`, across all
// states (open, closed, merged). It is the Gitea-derived replacement for
// the legacy prMappings table: callers who need to know "what PRs exist
// for this ticket" should use this instead of maintaining their own map.
//
// The result is NOT sorted; callers that care about chronology should
// order by PullRequest.Number.
func (c *Client) ListPullRequestsForTicket(owner, repo string, ticketID int) ([]PullRequest, error) {
	all, err := c.ListPullRequests(owner, repo, "all")
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf(ticketBranchPrefixFmt, ticketID)
	var matches []PullRequest
	for _, pr := range all {
		if pr.Head.Ref == "" {
			continue
		}
		// Exact-prefix match — a stray branch literally named `ticket-10`
		// should not look like a ticket-10 PR; only `ticket-10/...` does.
		if len(pr.Head.Ref) >= len(prefix) && pr.Head.Ref[:len(prefix)] == prefix {
			matches = append(matches, pr)
		}
	}
	return matches, nil
}

// GetPullRequest retrieves a single pull request.
func (c *Client) GetPullRequest(owner, repo string, number int) (*PullRequest, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", owner, repo, number)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get PR #%d: status %d", number, resp.StatusCode)
	}

	var pr PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("decoding PR: %w", err)
	}

	return &pr, nil
}

// --- PR Reviews ---

// Review represents a pull request review.
type Review struct {
	ID       int    `json:"id"`
	Body     string `json:"body"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}

// CreateReview posts a review on a pull request.
func (c *Client) CreateReview(owner, repo string, prNumber int, review *CreateReviewRequest) (*Review, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", owner, repo, prNumber)

	resp, err := c.doRequest(http.MethodPost, path, review)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create review: status %d, body: %s", resp.StatusCode, body)
	}

	var result Review
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding review: %w", err)
	}

	return &result, nil
}

// CreateReviewRequest holds parameters for creating a PR review.
type CreateReviewRequest struct {
	Body  string          `json:"body"`
	Event string          `json:"event"` // "APPROVED", "REQUEST_CHANGES", "COMMENT"
}

// --- PR Comments ---

// Comment represents a comment on an issue or PR.
type Comment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
}

// ListPRComments retrieves comments on a pull request.
func (c *Client) ListPRComments(owner, repo string, prNumber int) ([]Comment, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", owner, repo, prNumber)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list PR comments: status %d", resp.StatusCode)
	}

	var comments []Comment
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return nil, fmt.Errorf("decoding comments: %w", err)
	}

	return comments, nil
}

// --- Users ---

// GiteaUser represents a Gitea user.
type GiteaUser struct {
	ID       int    `json:"id"`
	Login    string `json:"login"`
	Email    string `json:"email"`
	IsAdmin  bool   `json:"is_admin"`
}

// CreateUser creates a new Gitea user (requires admin privileges).
func (c *Client) CreateUser(username, password, email string) (*GiteaUser, error) {
	payload := map[string]interface{}{
		"username":             username,
		"password":             password,
		"email":                email,
		"must_change_password": false,
	}

	resp, err := c.doRequest(http.MethodPost, "/api/v1/admin/users", payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create user: status %d, body: %s", resp.StatusCode, body)
	}

	var user GiteaUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("decoding user: %w", err)
	}

	return &user, nil
}

// GetUser retrieves a user by username. Returns nil if not found.
func (c *Client) GetUser(username string) (*GiteaUser, error) {
	path := fmt.Sprintf("/api/v1/users/%s", username)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get user: status %d", resp.StatusCode)
	}

	var user GiteaUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("decoding user: %w", err)
	}

	return &user, nil
}

// --- Branches ---

// Branch represents a Gitea branch.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// ListBranches lists branches for a repository.
func (c *Client) ListBranches(owner, repo string) ([]Branch, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/branches", owner, repo)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list branches: status %d", resp.StatusCode)
	}

	var branches []Branch
	if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
		return nil, fmt.Errorf("decoding branches: %w", err)
	}

	return branches, nil
}

// --- PR Editing ---

// EditPRRequest holds parameters for updating a pull request.
type EditPRRequest struct {
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

// EditPullRequest updates a pull request (assignees, title, body).
func (c *Client) EditPullRequest(owner, repo string, number int, opts *EditPRRequest) (*PullRequest, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", owner, repo, number)

	resp, err := c.doRequest(http.MethodPatch, path, opts)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("edit PR #%d: status %d, body: %s", number, resp.StatusCode, body)
	}

	var pr PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("decoding PR: %w", err)
	}

	return &pr, nil
}

// --- PR Diff ---

// GetPRDiff retrieves the diff for a pull request as a string.
func (c *Client) GetPRDiff(owner, repo string, number int) (string, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d.diff", owner, repo, number)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get PR diff #%d: status %d", number, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading diff: %w", err)
	}

	return string(data), nil
}

// --- PR Reviews (read) ---

// PRReview represents a review submitted on a pull request.
type PRReview struct {
	ID                int64  `json:"id"`
	Body              string `json:"body"`
	State             string `json:"state"` // "APPROVED", "REQUEST_CHANGES", "COMMENT", "PENDING"
	CommitID          string `json:"commit_id"`
	Stale             bool   `json:"stale"`
	CodeCommentsCount int    `json:"code_comments_count"`
	User              struct {
		Login string `json:"login"`
	} `json:"user"`
}

// GetPRReviews retrieves all reviews on a pull request.
func (c *Client) GetPRReviews(owner, repo string, prNumber int) ([]PRReview, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", owner, repo, prNumber)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get PR reviews #%d: status %d", prNumber, resp.StatusCode)
	}

	var reviews []PRReview
	if err := json.NewDecoder(resp.Body).Decode(&reviews); err != nil {
		return nil, fmt.Errorf("decoding reviews: %w", err)
	}

	return reviews, nil
}

// ReviewComment represents a code-level comment within a PR review.
type ReviewComment struct {
	ID       int64  `json:"id"`
	Body     string `json:"body"`
	Path     string `json:"path"`
	OldLine  int    `json:"old_position"`
	NewLine  int    `json:"new_position"`
	DiffHunk string `json:"diff_hunk"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

// GetPRReviewComments retrieves the code-level comments for a specific review.
func (c *Client) GetPRReviewComments(owner, repo string, prNumber int, reviewID int64) ([]ReviewComment, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews/%d/comments", owner, repo, prNumber, reviewID)

	resp, err := c.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get review comments for review %d on PR #%d: status %d", reviewID, prNumber, resp.StatusCode)
	}

	var comments []ReviewComment
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return nil, fmt.Errorf("decoding review comments: %w", err)
	}

	return comments, nil
}

// --- Webhooks ---

// CreateHookRequest holds parameters for creating a webhook.
type CreateHookRequest struct {
	Type   string            `json:"type"` // "gitea"
	Config map[string]string `json:"config"`
	Events []string          `json:"events"`
	Active bool              `json:"active"`
}

// AddCollaborator grants a user access to a repository. Requires the
// calling client to have owner or site-admin privileges on the repo;
// the orchestrator uses the configured Gitea admin credentials, which
// can add collaborators to any repo in the local Gitea. permission is
// one of "read", "write", "admin". The call is idempotent — repeating
// it for a user who is already a collaborator still returns 204.
func (c *Client) AddCollaborator(owner, repo, username, permission string) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/collaborators/%s", owner, repo, username)
	payload := map[string]string{"permission": permission}

	resp, err := c.doRequest(http.MethodPut, path, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("add collaborator %s to %s/%s: status %d, body: %s",
			username, owner, repo, resp.StatusCode, body)
	}
	return nil
}

// CreateRepoWebhook registers a webhook on a repository.
func (c *Client) CreateRepoWebhook(owner, repo string, hook *CreateHookRequest) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/hooks", owner, repo)

	resp, err := c.doRequest(http.MethodPost, path, hook)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create webhook on %s/%s: status %d, body: %s", owner, repo, resp.StatusCode, body)
	}

	return nil
}

// --- Internal helpers ---

// doRequest builds and executes an HTTP request with basic auth.
func (c *Client) doRequest(method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.httpClient.Do(req)
}
