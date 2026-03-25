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
