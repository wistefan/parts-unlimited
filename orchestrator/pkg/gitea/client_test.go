package gitea

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func setupTestServer(t *testing.T, handlers map[string]http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, handler := range handlers {
		mux.HandleFunc(pattern, handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "admin", "password")
	return srv, client
}

func TestCreateRepo(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/user/repos": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			user, pass, ok := r.BasicAuth()
			if !ok || user != "admin" || pass != "password" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "test-repo" {
				t.Errorf("expected name='test-repo', got %v", body["name"])
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(Repository{
				ID:       1,
				Name:     "test-repo",
				FullName: "admin/test-repo",
				CloneURL: "http://localhost:3000/admin/test-repo.git",
			})
		},
	})

	repo, err := client.CreateRepo("test-repo", "A test repo", false)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if repo.Name != "test-repo" {
		t.Errorf("expected name='test-repo', got %q", repo.Name)
	}
}

func TestGetRepo(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/myrepo": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(Repository{
				ID:            5,
				Name:          "myrepo",
				FullName:      "owner/myrepo",
				DefaultBranch: "main",
			})
		},
	})

	repo, err := client.GetRepo("owner", "myrepo")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo == nil {
		t.Fatal("expected non-nil repo")
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("expected branch='main', got %q", repo.DefaultBranch)
	}
}

func TestGetRepoNotFound(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/missing": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})

	repo, err := client.GetRepo("owner", "missing")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo != nil {
		t.Error("expected nil repo for not found")
	}
}

func TestCreatePullRequest(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			var body CreatePRRequest
			json.NewDecoder(r.Body).Decode(&body)
			if body.Title != "My PR" {
				t.Errorf("expected title='My PR', got %q", body.Title)
			}
			if body.Head != "feature" || body.Base != "main" {
				t.Errorf("expected head=feature base=main, got head=%s base=%s", body.Head, body.Base)
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(PullRequest{
				Number:  1,
				Title:   "My PR",
				HTMLURL: "http://localhost:3000/owner/repo/pulls/1",
			})
		},
	})

	pr, err := client.CreatePullRequest("owner", "repo", &CreatePRRequest{
		Title: "My PR",
		Body:  "Description",
		Head:  "feature",
		Base:  "main",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if pr.Number != 1 {
		t.Errorf("expected PR number=1, got %d", pr.Number)
	}
}

func TestListPullRequests(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("state") != "open" {
				t.Errorf("expected state=open, got %s", r.URL.Query().Get("state"))
			}
			json.NewEncoder(w).Encode([]PullRequest{
				{Number: 1, Title: "PR 1", State: "open"},
				{Number: 2, Title: "PR 2", State: "open"},
			})
		},
	})

	prs, err := client.ListPullRequests("owner", "repo", "open")
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(prs) != 2 {
		t.Errorf("expected 2 PRs, got %d", len(prs))
	}
}

func TestListPullRequestsForTicket(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("state") != "all" {
				t.Errorf("expected state=all, got %s", r.URL.Query().Get("state"))
			}
			json.NewEncoder(w).Encode([]PullRequest{
				{Number: 1, Title: "plan", Head: PRRef{Ref: "ticket-10/plan"}},
				{Number: 2, Title: "step 1", Head: PRRef{Ref: "ticket-10/step-1"}},
				{Number: 3, Title: "unrelated", Head: PRRef{Ref: "feature/x"}},
				{Number: 4, Title: "different ticket", Head: PRRef{Ref: "ticket-11/plan"}},
				{Number: 5, Title: "prefix lookalike", Head: PRRef{Ref: "ticket-100/plan"}},
				{Number: 6, Title: "bare name", Head: PRRef{Ref: "ticket-10"}},
				// Branch deleted after merge: Gitea rewrites Ref to the
				// synthetic pull ref, but Label keeps the branch name.
				{Number: 7, Title: "merged, branch deleted", Head: PRRef{Ref: "refs/pull/7/head", Label: "ticket-10/step-2"}},
				// Cross-repo PR: Label carries an "{owner}:" prefix.
				{Number: 8, Title: "cross-repo", Head: PRRef{Ref: "refs/pull/8/head", Label: "fork-owner:ticket-10/step-3"}},
				// Deleted branch belonging to a different ticket.
				{Number: 9, Title: "other ticket, deleted", Head: PRRef{Ref: "refs/pull/9/head", Label: "ticket-11/step-1"}},
			})
		},
	})

	prs, err := client.ListPullRequestsForTicket("owner", "repo", 10)
	if err != nil {
		t.Fatalf("ListPullRequestsForTicket: %v", err)
	}
	if len(prs) != 4 {
		t.Fatalf("expected 4 PRs for ticket 10, got %d: %+v", len(prs), prs)
	}
	gotNumbers := map[int]bool{}
	for _, pr := range prs {
		gotNumbers[pr.Number] = true
	}
	for _, want := range []int{1, 2, 7, 8} {
		if !gotNumbers[want] {
			t.Errorf("expected PR #%d in results, got %v", want, gotNumbers)
		}
	}
}

func TestCreateUser(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/admin/users": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["username"] != "newuser" {
				t.Errorf("expected username='newuser', got %v", body["username"])
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(GiteaUser{ID: 10, Login: "newuser"})
		},
	})

	user, err := client.CreateUser("newuser", "pass123", "new@example.com")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Login != "newuser" {
		t.Errorf("expected login='newuser', got %q", user.Login)
	}
}

func TestCreateReview(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls/1/reviews": func(w http.ResponseWriter, r *http.Request) {
			var body CreateReviewRequest
			json.NewDecoder(r.Body).Decode(&body)
			if body.Event != "COMMENT" {
				t.Errorf("expected event='COMMENT', got %q", body.Event)
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(Review{ID: 1, Body: body.Body, State: "COMMENT"})
		},
	})

	review, err := client.CreateReview("owner", "repo", 1, &CreateReviewRequest{
		Body:  "Looks good",
		Event: "COMMENT",
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if review.Body != "Looks good" {
		t.Errorf("expected body='Looks good', got %q", review.Body)
	}
}

func TestEditPullRequest(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls/5": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("expected PATCH, got %s", r.Method)
			}
			var body EditPRRequest
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Assignees) != 1 || body.Assignees[0] != "wistefan" {
				t.Errorf("expected assignees=[wistefan], got %v", body.Assignees)
			}
			json.NewEncoder(w).Encode(PullRequest{
				Number:    5,
				Assignees: []GiteaUser{{Login: "wistefan"}},
			})
		},
	})

	pr, err := client.EditPullRequest("owner", "repo", 5, &EditPRRequest{
		Assignees: []string{"wistefan"},
	})
	if err != nil {
		t.Fatalf("EditPullRequest: %v", err)
	}
	if pr.Number != 5 {
		t.Errorf("expected PR number=5, got %d", pr.Number)
	}
	if len(pr.Assignees) != 1 || pr.Assignees[0].Login != "wistefan" {
		t.Errorf("expected assignee wistefan, got %v", pr.Assignees)
	}
}

func TestGetPRDiff(t *testing.T) {
	expectedDiff := "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old\n+new\n"
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls/3.diff": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(expectedDiff))
		},
	})

	diff, err := client.GetPRDiff("owner", "repo", 3)
	if err != nil {
		t.Fatalf("GetPRDiff: %v", err)
	}
	if diff != expectedDiff {
		t.Errorf("expected diff content, got %q", diff)
	}
}

func TestGetPRReviews(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls/2/reviews": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			json.NewEncoder(w).Encode([]PRReview{
				{ID: 10, Body: "Please fix", State: "REQUEST_CHANGES"},
				{ID: 11, Body: "Looks good", State: "APPROVED"},
			})
		},
	})

	reviews, err := client.GetPRReviews("owner", "repo", 2)
	if err != nil {
		t.Fatalf("GetPRReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("expected 2 reviews, got %d", len(reviews))
	}
	if reviews[0].State != "REQUEST_CHANGES" {
		t.Errorf("expected first review state=REQUEST_CHANGES, got %q", reviews[0].State)
	}
}

func TestGetPRReviewComments(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls/2/reviews/10/comments": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]ReviewComment{
				{ID: 100, Body: "Fix this line", Path: "main.go", NewLine: 42},
			})
		},
	})

	comments, err := client.GetPRReviewComments("owner", "repo", 2, 10)
	if err != nil {
		t.Fatalf("GetPRReviewComments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].Path != "main.go" {
		t.Errorf("expected path=main.go, got %q", comments[0].Path)
	}
	if comments[0].NewLine != 42 {
		t.Errorf("expected line=42, got %d", comments[0].NewLine)
	}
}

func TestCreateRepoWebhook(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/claude/test-repo/hooks": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			var body CreateHookRequest
			json.NewDecoder(r.Body).Decode(&body)
			if body.Type != "gitea" {
				t.Errorf("expected type=gitea, got %q", body.Type)
			}
			if len(body.Events) != 2 {
				t.Errorf("expected 2 events, got %d", len(body.Events))
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("{}"))
		},
	})

	err := client.CreateRepoWebhook("claude", "test-repo", &CreateHookRequest{
		Type: "gitea",
		Config: map[string]string{
			"url":          "http://orchestrator:8080/webhooks/gitea",
			"content_type": "json",
		},
		Events: []string{"pull_request", "pull_request_review"},
		Active: true,
	})
	if err != nil {
		t.Fatalf("CreateRepoWebhook: %v", err)
	}
}

// TestGetPullRequestParsesHeadSHA pins the head-SHA field the reconciler
// uses to decide whether a PR has gained commits since its last
// automated review. Without it every PR looks unreviewed forever.
func TestGetPullRequestParsesHeadSHA(t *testing.T) {
	const wantSHA = "0a1b2c3d4e5f60718293a4b5c6d7e8f901234567"

	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls/1": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			// Raw JSON rather than the struct: the point is that the
			// wire field `head.sha` is decoded.
			w.Write([]byte(`{
				"id": 1, "number": 1, "title": "Step 1", "state": "open",
				"head": {"label": "ticket-1/step-1", "ref": "ticket-1/step-1", "sha": "` + wantSHA + `"},
				"base": {"label": "ticket-1/work", "ref": "ticket-1/work", "sha": "aaaa"}
			}`))
		},
	})

	pr, err := client.GetPullRequest("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if pr.Head.Sha != wantSHA {
		t.Errorf("head SHA: got %q, want %q", pr.Head.Sha, wantSHA)
	}
	if pr.Base.Sha != "aaaa" {
		t.Errorf("base SHA: got %q, want %q", pr.Base.Sha, "aaaa")
	}
}

// TestCreateReviewWithInlineComments checks that inline code comments
// reach Gitea in the shape its reviews API expects.
func TestCreateReviewWithInlineComments(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls/1/reviews": func(w http.ResponseWriter, r *http.Request) {
			var body CreateReviewRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decoding request: %v", err)
			}
			if body.Event != "COMMENT" {
				t.Errorf("expected event='COMMENT', got %q", body.Event)
			}
			if len(body.Comments) != 1 {
				t.Fatalf("expected 1 inline comment, got %d", len(body.Comments))
			}
			c := body.Comments[0]
			if c.Path != "pkg/foo/bar.go" {
				t.Errorf("path: got %q", c.Path)
			}
			if c.NewLine != 42 {
				t.Errorf("new_position: got %d, want 42", c.NewLine)
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(Review{ID: 1, Body: body.Body, State: "COMMENT"})
		},
	})

	_, err := client.CreateReview("owner", "repo", 1, &CreateReviewRequest{
		Body:  "Summary",
		Event: "COMMENT",
		Comments: []CreateReviewComment{
			{Path: "pkg/foo/bar.go", Body: "This can overflow.", NewLine: 42},
		},
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
}

// TestCreateReviewOmitsEmptyComments guards the summary-only case: an
// empty Comments slice must not appear in the payload at all, since
// Gitea rejects an empty comments array on some versions.
func TestCreateReviewOmitsEmptyComments(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/repos/owner/repo/pulls/1/reviews": func(w http.ResponseWriter, r *http.Request) {
			var raw map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				t.Fatalf("decoding request: %v", err)
			}
			if _, present := raw["comments"]; present {
				t.Errorf("comments key should be omitted when empty, got %v", raw["comments"])
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(Review{ID: 1, State: "COMMENT"})
		},
	})

	if _, err := client.CreateReview("owner", "repo", 1, &CreateReviewRequest{
		Body:  "Summary only",
		Event: "COMMENT",
	}); err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
}
