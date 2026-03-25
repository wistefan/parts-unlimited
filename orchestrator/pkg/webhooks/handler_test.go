package webhooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func signPayload(secret string, body []byte) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	return "sha1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookHandler_TestEvent(t *testing.T) {
	h := NewHandler("")

	body, _ := json.Marshal(map[string]string{"action": "test", "type": "test"})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for test event, got %d", w.Code)
	}
}

func TestWebhookHandler_MethodNotAllowed(t *testing.T) {
	h := NewHandler("")

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestWebhookHandler_HMACVerification(t *testing.T) {
	secret := "test-secret"
	h := NewHandler(secret)

	body, _ := json.Marshal(map[string]string{"action": "test", "type": "test"})

	t.Run("valid signature", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature", signPayload(secret, body))
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 with valid sig, got %d", w.Code)
		}
	})

	t.Run("invalid signature", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature", "sha1=invalid")
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 with invalid sig, got %d", w.Code)
		}
	})

	t.Run("missing signature", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		w := httptest.NewRecorder()

		h.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 with missing sig, got %d", w.Code)
		}
	})
}

func TestWebhookHandler_EventDispatching(t *testing.T) {
	h := NewHandler("")

	var exactCalled, wildcardCalled, catchAllCalled bool

	h.OnFunc("userstory", "change", func(e *WebhookEvent) error {
		exactCalled = true
		return nil
	})

	h.OnFunc("userstory", "*", func(e *WebhookEvent) error {
		wildcardCalled = true
		return nil
	})

	h.OnAll(EventHandlerFunc(func(e *WebhookEvent) error {
		catchAllCalled = true
		return nil
	}))

	event := WebhookEvent{
		Action: "change",
		Type:   "userstory",
		By:     WebhookUser{Username: "test"},
	}
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !exactCalled {
		t.Error("exact handler not called")
	}
	if !wildcardCalled {
		t.Error("wildcard handler not called")
	}
	if !catchAllCalled {
		t.Error("catch-all handler not called")
	}
}

func TestWebhookHandler_NoMatchingHandler(t *testing.T) {
	h := NewHandler("")

	h.OnFunc("task", "create", func(e *WebhookEvent) error {
		t.Error("should not be called for userstory events")
		return nil
	})

	event := WebhookEvent{Action: "create", Type: "userstory"}
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestParseUserStoryData(t *testing.T) {
	event := &WebhookEvent{
		Data: json.RawMessage(`{
			"id": 42,
			"ref": 5,
			"subject": "Fix login",
			"status": {"id": 1, "name": "ready", "slug": "ready"},
			"tags": ["frontend", "delegate:test"],
			"assigned_users": [10, 20],
			"project": {"id": 1, "name": "Dev"}
		}`),
	}

	data, err := ParseUserStoryData(event)
	if err != nil {
		t.Fatalf("ParseUserStoryData: %v", err)
	}

	if data.ID != 42 {
		t.Errorf("expected ID=42, got %d", data.ID)
	}
	if data.Subject != "Fix login" {
		t.Errorf("expected subject 'Fix login', got %q", data.Subject)
	}
	if data.Status.Name != "ready" {
		t.Errorf("expected status 'ready', got %q", data.Status.Name)
	}
	if len(data.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(data.Tags))
	}
	if len(data.AssignedUsers) != 2 {
		t.Errorf("expected 2 assigned users, got %d", len(data.AssignedUsers))
	}
}

func TestParseStatusChange(t *testing.T) {
	t.Run("with status change", func(t *testing.T) {
		event := &WebhookEvent{
			Change: &WebhookChange{
				Diff: json.RawMessage(`{"status": {"from": "new", "to": "ready"}}`),
			},
		}

		change, err := ParseStatusChange(event)
		if err != nil {
			t.Fatalf("ParseStatusChange: %v", err)
		}
		if change == nil {
			t.Fatal("expected non-nil status change")
		}
		if change.From != "new" || change.To != "ready" {
			t.Errorf("expected new->ready, got %s->%s", change.From, change.To)
		}
	})

	t.Run("no change field", func(t *testing.T) {
		event := &WebhookEvent{}
		change, err := ParseStatusChange(event)
		if err != nil {
			t.Fatalf("ParseStatusChange: %v", err)
		}
		if change != nil {
			t.Error("expected nil for event without change")
		}
	})

	t.Run("change without status", func(t *testing.T) {
		event := &WebhookEvent{
			Change: &WebhookChange{
				Diff: json.RawMessage(`{"assigned_to": {"from": null, "to": "agent-1"}}`),
			},
		}
		change, err := ParseStatusChange(event)
		if err != nil {
			t.Fatalf("ParseStatusChange: %v", err)
		}
		if change != nil {
			t.Error("expected nil when no status in diff")
		}
	})
}
