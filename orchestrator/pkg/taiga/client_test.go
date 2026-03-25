package taiga

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
	client := NewClient(srv.URL)
	return srv, client
}

func TestAuthenticate(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/auth": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["username"] != "admin" || body["password"] != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"auth_token": "test-token-123",
			})
		},
	})

	if err := client.Authenticate("admin", "secret"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	client.mu.RLock()
	if client.authToken != "test-token-123" {
		t.Errorf("expected token 'test-token-123', got %q", client.authToken)
	}
	client.mu.RUnlock()
}

func TestAuthenticateFailure(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/auth": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	})

	if err := client.Authenticate("bad", "creds"); err == nil {
		t.Error("expected error for failed auth")
	}
}

func TestListUserStories(t *testing.T) {
	stories := []UserStory{
		{ID: 1, Ref: 1, Subject: "First story", Status: 1, Project: 1},
		{ID: 2, Ref: 2, Subject: "Second story", Status: 2, Project: 1},
	}

	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/auth": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"auth_token": "tok"})
		},
		"/api/v1/userstories": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("project") != "1" {
				t.Errorf("expected project=1, got %s", r.URL.Query().Get("project"))
			}
			json.NewEncoder(w).Encode(stories)
		},
	})

	client.Authenticate("admin", "pass")

	result, err := client.ListUserStories(1, nil)
	if err != nil {
		t.Fatalf("ListUserStories: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("expected 2 stories, got %d", len(result))
	}
	if result[0].Subject != "First story" {
		t.Errorf("expected 'First story', got %q", result[0].Subject)
	}
}

func TestGetUserStory(t *testing.T) {
	story := UserStory{ID: 42, Ref: 5, Subject: "My story", Version: 3}

	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/auth": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"auth_token": "tok"})
		},
		"/api/v1/userstories/42": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(story)
		},
	})

	client.Authenticate("admin", "pass")

	result, err := client.GetUserStory(42)
	if err != nil {
		t.Fatalf("GetUserStory: %v", err)
	}
	if result.ID != 42 {
		t.Errorf("expected ID=42, got %d", result.ID)
	}
	if result.Version != 3 {
		t.Errorf("expected Version=3, got %d", result.Version)
	}
}

func TestUpdateUserStory(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/auth": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"auth_token": "tok"})
		},
		"/api/v1/userstories/10": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("expected PATCH, got %s", r.Method)
			}
			var update UserStoryUpdate
			json.NewDecoder(r.Body).Decode(&update)
			if update.Version != 5 {
				t.Errorf("expected version=5, got %d", update.Version)
			}
			json.NewEncoder(w).Encode(UserStory{ID: 10, Status: 2, Version: 6})
		},
	})

	client.Authenticate("admin", "pass")

	statusID := 2
	result, err := client.UpdateUserStory(10, &UserStoryUpdate{
		Status:  &statusID,
		Version: 5,
	})
	if err != nil {
		t.Fatalf("UpdateUserStory: %v", err)
	}
	if result.Status != 2 {
		t.Errorf("expected Status=2, got %d", result.Status)
	}
}

func TestListStatuses(t *testing.T) {
	statuses := []Status{
		{ID: 1, Name: "ready", Slug: "ready", Project: 1},
		{ID: 2, Name: "in progress", Slug: "in-progress", Project: 1},
	}

	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/auth": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"auth_token": "tok"})
		},
		"/api/v1/userstory-statuses": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(statuses)
		},
	})

	client.Authenticate("admin", "pass")

	result, err := client.ListStatuses(1)
	if err != nil {
		t.Fatalf("ListStatuses: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("expected 2 statuses, got %d", len(result))
	}
}

func TestNotAuthenticated(t *testing.T) {
	client := NewClient("http://localhost:9999")

	_, err := client.ListUserStories(1, nil)
	if err == nil {
		t.Error("expected error when not authenticated")
	}
}

func TestCreateWebhook(t *testing.T) {
	_, client := setupTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/auth": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"auth_token": "tok"})
		},
		"/api/v1/webhooks": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(Webhook{ID: 1, Project: 1, Name: "test", URL: "http://example.com"})
		},
	})

	client.Authenticate("admin", "pass")

	wh, err := client.CreateWebhook(1, "test", "http://example.com", "secret")
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if wh.ID != 1 {
		t.Errorf("expected webhook ID=1, got %d", wh.ID)
	}
}
