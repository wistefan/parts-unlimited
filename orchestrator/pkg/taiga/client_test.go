package taiga

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// expiringTokenServer simulates Taiga handing out access tokens that expire:
// only the most recently issued token is accepted, every older one gets a 401.
// It reports how often each endpoint was hit so tests can assert that a
// rejected request is replayed exactly once.
type expiringTokenServer struct {
	mu          sync.Mutex
	validToken  string
	issued      int
	logins      int
	apiCalls    int
	lastBody    string
	lastHeaders http.Header
}

// expire invalidates the token currently held by the client, as Taiga does
// once ACCESS_TOKEN_LIFETIME has passed.
func (s *expiringTokenServer) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validToken = "expired-by-test"
}

func (s *expiringTokenServer) handler(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.logins++
		s.issued++
		s.validToken = fmt.Sprintf("token-%d", s.issued)
		token := s.validToken
		s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"auth_token": token})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.apiCalls++
		s.lastBody = string(body)
		s.lastHeaders = r.Header.Clone()
		valid := s.validToken
		s.mu.Unlock()

		if r.Header.Get("Authorization") != "Bearer "+valid {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(payload))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestReauthenticatesOnExpiredToken(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		call    func(*Client) error
		wantReq string
	}{
		{
			name:    "GET without body",
			payload: `[{"id": 1, "ref": 7, "subject": "Story"}]`,
			call: func(c *Client) error {
				_, err := c.ListUserStories(1, nil)
				return err
			},
		},
		{
			name:    "PATCH replays the request body",
			payload: `{"id": 1, "ref": 7, "subject": "Story"}`,
			call: func(c *Client) error {
				_, err := c.UpdateUserStory(1, &UserStoryUpdate{Comment: "hello", Version: 3})
				return err
			},
			wantReq: `"comment":"hello"`,
		},
		{
			name:    "GetComments keeps its extra headers",
			payload: `[]`,
			call: func(c *Client) error {
				_, err := c.GetComments(1)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &expiringTokenServer{}
			srv := state.handler(t, tt.payload)
			client := NewClient(srv.URL)

			if err := client.Authenticate("admin", "secret"); err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			state.expire()

			if err := tt.call(client); err != nil {
				t.Fatalf("call after token expiry: %v", err)
			}

			state.mu.Lock()
			defer state.mu.Unlock()
			if state.logins != 2 {
				t.Errorf("expected 2 logins (initial + refresh), got %d", state.logins)
			}
			if state.apiCalls != 2 {
				t.Errorf("expected the request to be replayed exactly once, got %d attempts", state.apiCalls)
			}
			if tt.wantReq != "" && !strings.Contains(state.lastBody, tt.wantReq) {
				t.Errorf("replayed body %q does not contain %q", state.lastBody, tt.wantReq)
			}
			if state.lastHeaders.Get("Authorization") != "Bearer "+state.validToken {
				t.Errorf("replay used %q, want the refreshed token %q",
					state.lastHeaders.Get("Authorization"), state.validToken)
			}
		})
	}
}

func TestGetCommentsSendsDisablePaginationOnReplay(t *testing.T) {
	state := &expiringTokenServer{}
	srv := state.handler(t, `[]`)
	client := NewClient(srv.URL)

	if err := client.Authenticate("admin", "secret"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	state.expire()

	if _, err := client.GetComments(1); err != nil {
		t.Fatalf("GetComments: %v", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if got := state.lastHeaders.Get(disablePaginationHeader); got != disablePaginationValue {
		t.Errorf("replay sent %s=%q, want %q", disablePaginationHeader, got, disablePaginationValue)
	}
}

func TestReauthenticationFailureIsNotRetriedForever(t *testing.T) {
	var logins, apiCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth", func(w http.ResponseWriter, r *http.Request) {
		logins++
		if logins == 1 {
			json.NewEncoder(w).Encode(map[string]string{"auth_token": "first"})
			return
		}
		// Password changed in the meantime: the refresh cannot succeed.
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL)
	if err := client.Authenticate("admin", "secret"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if _, err := client.ListUserStories(1, nil); err == nil {
		t.Fatal("expected an error when re-authentication fails")
	}
	if logins != 2 {
		t.Errorf("expected exactly one refresh attempt, got %d logins", logins)
	}
	if apiCalls != 1 {
		t.Errorf("expected no replay after a failed refresh, got %d attempts", apiCalls)
	}
}

func TestRefreshTokenSkipsWhenAnotherCallerAlreadyRefreshed(t *testing.T) {
	state := &expiringTokenServer{}
	srv := state.handler(t, `[]`)
	client := NewClient(srv.URL)

	if err := client.Authenticate("admin", "secret"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// A concurrent caller already replaced the token this goroutine used.
	if err := client.refreshToken("some-older-token"); err != nil {
		t.Fatalf("refreshToken: %v", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.logins != 1 {
		t.Errorf("expected no additional login, got %d", state.logins)
	}
}

func TestRefreshTokenWithoutCredentials(t *testing.T) {
	client := NewClient("http://taiga.invalid")
	if err := client.refreshToken(""); err == nil {
		t.Error("expected an error when no credentials were ever supplied")
	}
}
