package webhooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func giteaSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestGiteaHandler_PROpened(t *testing.T) {
	secret := "test-secret"
	handler := NewGiteaHandler(secret)

	var received string
	handler.On(GiteaEventPROpened, func(eventType string, event *GiteaPREvent) error {
		received = eventType
		if event.PullRequest.Number != 7 {
			t.Errorf("expected PR #7, got #%d", event.PullRequest.Number)
		}
		if event.Repository.FullName != "claude/test-repo" {
			t.Errorf("expected repo claude/test-repo, got %s", event.Repository.FullName)
		}
		return nil
	})

	payload := GiteaPREvent{
		Action: "opened",
		Number: 7,
		PullRequest: GiteaPR{
			Number: 7,
			Title:  "Ticket #42: Implementation Plan",
			State:  "open",
		},
		Repository: GiteaRepo{FullName: "claude/test-repo"},
		Sender:     GiteaSender{Login: "general-agent-1"},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "pull_request")
	req.Header.Set("X-Gitea-Signature", giteaSignature(secret, body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if received != GiteaEventPROpened {
		t.Errorf("expected event %s, got %q", GiteaEventPROpened, received)
	}
}

func TestGiteaHandler_PRMerged(t *testing.T) {
	handler := NewGiteaHandler("")

	var received string
	handler.On(GiteaEventPRMerged, func(eventType string, event *GiteaPREvent) error {
		received = eventType
		return nil
	})

	payload := GiteaPREvent{
		Action:      "closed",
		Number:      5,
		PullRequest: GiteaPR{Number: 5, Merged: true, State: "closed"},
		Repository:  GiteaRepo{FullName: "claude/repo"},
		Sender:      GiteaSender{Login: "wistefan"},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "pull_request")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if received != GiteaEventPRMerged {
		t.Errorf("expected %s, got %q", GiteaEventPRMerged, received)
	}
}

func TestGiteaHandler_PRClosedWithoutMerge(t *testing.T) {
	handler := NewGiteaHandler("")

	var received string
	handler.On(GiteaEventPRClosed, func(eventType string, event *GiteaPREvent) error {
		received = eventType
		return nil
	})

	payload := GiteaPREvent{
		Action:      "closed",
		Number:      3,
		PullRequest: GiteaPR{Number: 3, Merged: false, State: "closed"},
		Repository:  GiteaRepo{FullName: "claude/repo"},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "pull_request")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if received != GiteaEventPRClosed {
		t.Errorf("expected %s, got %q", GiteaEventPRClosed, received)
	}
}

func TestGiteaHandler_ReviewRequestChanges(t *testing.T) {
	handler := NewGiteaHandler("")

	var received string
	handler.On(GiteaEventReviewRequestChanges, func(eventType string, event *GiteaPREvent) error {
		received = eventType
		if event.Review.Body != "Please fix the error handling" {
			t.Errorf("unexpected review body: %q", event.Review.Body)
		}
		return nil
	})

	payload := GiteaPREvent{
		Action:      "submitted",
		Number:      1,
		PullRequest: GiteaPR{Number: 1},
		Repository:  GiteaRepo{FullName: "claude/repo"},
		Sender:      GiteaSender{Login: "wistefan"},
		Review: &GiteaReview{
			ID:    10,
			Body:  "Please fix the error handling",
			State: "REQUEST_CHANGES",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "pull_request_review")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if received != GiteaEventReviewRequestChanges {
		t.Errorf("expected %s, got %q", GiteaEventReviewRequestChanges, received)
	}
}

func TestGiteaHandler_InvalidSignature(t *testing.T) {
	handler := NewGiteaHandler("real-secret")

	body := []byte(`{"action":"opened"}`)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "pull_request")
	req.Header.Set("X-Gitea-Signature", "wrong-signature")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestGiteaHandler_UnhandledEvent(t *testing.T) {
	handler := NewGiteaHandler("")

	body := []byte(`{"action":"edited","number":1,"pull_request":{"number":1},"repository":{"full_name":"x/y"},"sender":{"login":"a"}}`)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "pull_request")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Unhandled events should still return 200
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestParseTicketIDFromPRBody(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		body     string
		expected int
	}{
		{
			name:     "ticket in title",
			title:    "Ticket #42: Implementation Plan",
			body:     "",
			expected: 42,
		},
		{
			name:     "ticket in body",
			title:    "Some PR",
			body:     "Fixes Ticket #99 by adding tests",
			expected: 99,
		},
		{
			name:     "ticket from branch name in body",
			title:    "Some PR",
			body:     "Branch: ticket-55/step-3",
			expected: 55,
		},
		{
			name:     "no ticket reference",
			title:    "Random PR",
			body:     "Just a regular change",
			expected: 0,
		},
		{
			name:     "branch in title with mode suffix",
			title:    "ticket-12/plan: initial implementation plan",
			body:     "",
			expected: 12,
		},
		{
			name:     "branch in title with step",
			title:    "ticket-12/step-3: add tests",
			body:     "",
			expected: 12,
		},
		{
			name:     "closes ticket-N work in body",
			title:    "Some PR",
			body:     "Closes ticket-7/work",
			expected: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTicketIDFromPRBody(tt.title, tt.body)
			if got != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, got)
			}
		})
	}
}
