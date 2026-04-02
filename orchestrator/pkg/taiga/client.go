// Package taiga provides a client for the Taiga REST API.
package taiga

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Client is a Taiga REST API client.
type Client struct {
	baseURL    string
	httpClient *http.Client

	mu        sync.RWMutex
	authToken string
}

// NewClient creates a new Taiga API client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Authenticate obtains a JWT token using username/password credentials.
func (c *Client) Authenticate(username, password string) error {
	payload := map[string]string{
		"type":     "normal",
		"username": username,
		"password": password,
	}

	resp, err := c.doRequest(http.MethodPost, "/api/v1/auth", payload, false)
	if err != nil {
		return fmt.Errorf("authentication request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authentication failed: status %d", resp.StatusCode)
	}

	var result struct {
		AuthToken string `json:"auth_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding auth response: %w", err)
	}

	c.mu.Lock()
	c.authToken = result.AuthToken
	c.mu.Unlock()

	return nil
}

// --- User Stories ---

// UserStory represents a Taiga user story.
type UserStory struct {
	ID            int              `json:"id"`
	Ref           int              `json:"ref"`
	Subject       string           `json:"subject"`
	Description   string           `json:"description"`
	Status        int              `json:"status"`
	AssignedTo    *int             `json:"assigned_to"`
	AssignedUsers []int            `json:"assigned_users"`
	Tags          [][]string       `json:"tags"`
	Project       int              `json:"project"`
	Version       int              `json:"version"`
	CreatedDate   string           `json:"created_date"`
	ModifiedDate  string           `json:"modified_date"`
}

// ListUserStories returns user stories for a project, optionally filtered by status or tags.
func (c *Client) ListUserStories(projectID int, opts *UserStoryListOptions) ([]UserStory, error) {
	path := fmt.Sprintf("/api/v1/userstories?project=%d", projectID)
	if opts != nil {
		if opts.StatusID > 0 {
			path += fmt.Sprintf("&status=%d", opts.StatusID)
		}
		if opts.Tags != "" {
			path += fmt.Sprintf("&tags=%s", opts.Tags)
		}
	}

	resp, err := c.doRequest(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list user stories: status %d", resp.StatusCode)
	}

	var stories []UserStory
	if err := json.NewDecoder(resp.Body).Decode(&stories); err != nil {
		return nil, fmt.Errorf("decoding user stories: %w", err)
	}

	return stories, nil
}

// UserStoryListOptions holds optional filters for listing user stories.
type UserStoryListOptions struct {
	StatusID int
	Tags     string
}

// GetUserStory retrieves a single user story by ID.
func (c *Client) GetUserStory(id int) (*UserStory, error) {
	path := fmt.Sprintf("/api/v1/userstories/%d", id)

	resp, err := c.doRequest(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get user story %d: status %d", id, resp.StatusCode)
	}

	var story UserStory
	if err := json.NewDecoder(resp.Body).Decode(&story); err != nil {
		return nil, fmt.Errorf("decoding user story: %w", err)
	}

	return &story, nil
}

// UpdateUserStory patches a user story. Only non-nil fields in the update are sent.
func (c *Client) UpdateUserStory(id int, update *UserStoryUpdate) (*UserStory, error) {
	path := fmt.Sprintf("/api/v1/userstories/%d", id)

	resp, err := c.doRequest(http.MethodPatch, path, update, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("update user story %d: status %d, body: %s", id, resp.StatusCode, body)
	}

	var story UserStory
	if err := json.NewDecoder(resp.Body).Decode(&story); err != nil {
		return nil, fmt.Errorf("decoding updated user story: %w", err)
	}

	return &story, nil
}

// UserStoryUpdate holds fields for patching a user story.
// Only include fields that should be changed.
type UserStoryUpdate struct {
	Status        *int       `json:"status,omitempty"`
	AssignedTo    *int       `json:"assigned_to,omitempty"`
	AssignedUsers []int      `json:"assigned_users,omitempty"`
	Tags          [][]string `json:"tags,omitempty"`
	Comment       string     `json:"comment,omitempty"`
	Version       int        `json:"version"`
}

// --- Comments ---

// AddComment adds a comment to a user story by patching it with a comment field.
func (c *Client) AddComment(storyID int, comment string, version int) error {
	update := &UserStoryUpdate{
		Comment: comment,
		Version: version,
	}

	_, err := c.UpdateUserStory(storyID, update)
	return err
}

// HistoryEntry represents an entry in a user story's history.
type HistoryEntry struct {
	ID                string  `json:"id"`
	Comment           string  `json:"comment"`
	CreatedAt         string  `json:"created_at"`
	DeleteCommentDate *string `json:"delete_comment_date"`
	User              struct {
		Username string `json:"username"`
	} `json:"user"`
}

// GetComments retrieves comment history for a user story.
// Results are sorted newest-first by created_at so callers can iterate
// from index 0 to find the most recent lifecycle marker.
func (c *Client) GetComments(storyID int) ([]HistoryEntry, error) {
	path := fmt.Sprintf("/api/v1/history/userstory/%d?type=comment", storyID)

	resp, err := c.doRequest(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get comments for story %d: status %d", storyID, resp.StatusCode)
	}

	var entries []HistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decoding comments: %w", err)
	}

	// Filter out deleted comments and sort newest-first.
	filtered := entries[:0]
	for _, e := range entries {
		if e.DeleteCommentDate != nil {
			continue
		}
		filtered = append(filtered, e)
	}

	// Sort newest-first by created_at (RFC 3339 strings sort lexicographically).
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt > filtered[j].CreatedAt
	})

	return filtered, nil
}

// --- Statuses ---

// Status represents a user story status.
type Status struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	IsClosed bool   `json:"is_closed"`
	Order    int    `json:"order"`
	Project  int    `json:"project"`
}

// ListStatuses returns all user story statuses for a project.
func (c *Client) ListStatuses(projectID int) ([]Status, error) {
	path := fmt.Sprintf("/api/v1/userstory-statuses?project=%d", projectID)

	resp, err := c.doRequest(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list statuses: status %d", resp.StatusCode)
	}

	var statuses []Status
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		return nil, fmt.Errorf("decoding statuses: %w", err)
	}

	return statuses, nil
}

// --- Users ---

// User represents a Taiga user.
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	IsActive bool   `json:"is_active"`
}

// ListProjectMembers returns users who are members of a project.
func (c *Client) ListProjectMembers(projectID int) ([]User, error) {
	path := fmt.Sprintf("/api/v1/users?project=%d", projectID)

	resp, err := c.doRequest(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list project members: status %d", resp.StatusCode)
	}

	var users []User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("decoding users: %w", err)
	}

	return users, nil
}

// RegisterUser creates a new user via Taiga's public registration endpoint.
// Requires public registration to be enabled on the Taiga instance.
func (c *Client) RegisterUser(username, password, email, fullName string) (*User, error) {
	payload := map[string]string{
		"type":      "public",
		"username":  username,
		"password":  password,
		"email":     email,
		"full_name": fullName,
		"accepted_terms": "true",
	}

	resp, err := c.doRequest(http.MethodPost, "/api/v1/auth/register", payload, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register user %q: status %d, body: %s", username, resp.StatusCode, body)
	}

	var result struct {
		ID       int    `json:"id"`
		Username string `json:"username"`
		Email    string `json:"email"`
		FullName string `json:"full_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding registered user: %w", err)
	}

	return &User{
		ID:       result.ID,
		Username: result.Username,
		Email:    result.Email,
		FullName: result.FullName,
		IsActive: true,
	}, nil
}

// --- Webhooks ---

// Webhook represents a Taiga webhook configuration.
type Webhook struct {
	ID      int    `json:"id"`
	Project int    `json:"project"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Key     string `json:"key"`
}

// CreateWebhook registers a webhook for a project.
func (c *Client) CreateWebhook(projectID int, name, url, key string) (*Webhook, error) {
	payload := map[string]interface{}{
		"project": projectID,
		"name":    name,
		"url":     url,
		"key":     key,
	}

	resp, err := c.doRequest(http.MethodPost, "/api/v1/webhooks", payload, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create webhook: status %d, body: %s", resp.StatusCode, body)
	}

	var wh Webhook
	if err := json.NewDecoder(resp.Body).Decode(&wh); err != nil {
		return nil, fmt.Errorf("decoding webhook: %w", err)
	}

	return &wh, nil
}

// ListWebhooks returns all webhooks for a project.
func (c *Client) ListWebhooks(projectID int) ([]Webhook, error) {
	path := fmt.Sprintf("/api/v1/webhooks?project=%d", projectID)

	resp, err := c.doRequest(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list webhooks: status %d", resp.StatusCode)
	}

	var webhooks []Webhook
	if err := json.NewDecoder(resp.Body).Decode(&webhooks); err != nil {
		return nil, fmt.Errorf("decoding webhooks: %w", err)
	}

	return webhooks, nil
}

// DeleteWebhook removes a webhook by ID.
func (c *Client) DeleteWebhook(webhookID int) error {
	path := fmt.Sprintf("/api/v1/webhooks/%d", webhookID)

	resp, err := c.doRequest(http.MethodDelete, path, nil, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete webhook %d: status %d", webhookID, resp.StatusCode)
	}

	return nil
}

// --- Projects ---

// Project represents a Taiga project.
type Project struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// GetProjectBySlug retrieves a project by its slug.
func (c *Client) GetProjectBySlug(slug string) (*Project, error) {
	path := fmt.Sprintf("/api/v1/resolver?project=%s", slug)

	resp, err := c.doRequest(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resolve project slug %q: status %d", slug, resp.StatusCode)
	}

	var resolved struct {
		Project int `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resolved); err != nil {
		return nil, fmt.Errorf("decoding resolved project: %w", err)
	}

	// Fetch full project
	projResp, err := c.doRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d", resolved.Project), nil, true)
	if err != nil {
		return nil, err
	}
	defer projResp.Body.Close()

	var project Project
	if err := json.NewDecoder(projResp.Body).Decode(&project); err != nil {
		return nil, fmt.Errorf("decoding project: %w", err)
	}

	return &project, nil
}

// --- Memberships ---

// Membership represents a project membership.
type Membership struct {
	ID      int    `json:"id"`
	User    int    `json:"user"`
	Project int    `json:"project"`
	Role    int    `json:"role"`
}

// CreateMembership adds a user to a project with a given role.
func (c *Client) CreateMembership(projectID, roleID int, username string) (*Membership, error) {
	payload := map[string]interface{}{
		"project":  projectID,
		"role":     roleID,
		"username": username,
	}

	resp, err := c.doRequest(http.MethodPost, "/api/v1/memberships", payload, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create membership: status %d, body: %s", resp.StatusCode, body)
	}

	var m Membership
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding membership: %w", err)
	}

	return &m, nil
}

// --- Roles ---

// Role represents a Taiga project role.
type Role struct {
	ID          int      `json:"id"`
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
	Project     int      `json:"project"`
}

// ListRoles returns all roles for a project.
func (c *Client) ListRoles(projectID int) ([]Role, error) {
	path := fmt.Sprintf("/api/v1/roles?project=%d", projectID)

	resp, err := c.doRequest(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list roles: status %d", resp.StatusCode)
	}

	var roles []Role
	if err := json.NewDecoder(resp.Body).Decode(&roles); err != nil {
		return nil, fmt.Errorf("decoding roles: %w", err)
	}

	return roles, nil
}

// --- Internal helpers ---

// doRequest builds and executes an HTTP request.
func (c *Client) doRequest(method, path string, body interface{}, auth bool) (*http.Response, error) {
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

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if auth {
		c.mu.RLock()
		token := c.authToken
		c.mu.RUnlock()

		if token == "" {
			return nil, fmt.Errorf("not authenticated: call Authenticate first")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	return c.httpClient.Do(req)
}
