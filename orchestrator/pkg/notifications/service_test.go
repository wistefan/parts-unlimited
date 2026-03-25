package notifications

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotify_StoresEvent(t *testing.T) {
	svc := NewService(Config{})

	svc.Notify(EventEscalation, 42, "agent-1", "Test", "msg", "")

	events := svc.GetEvents("", 10)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != EventEscalation {
		t.Errorf("expected type escalation, got %q", events[0].Type)
	}
	if events[0].TicketID != 42 {
		t.Errorf("expected ticketID=42, got %d", events[0].TicketID)
	}
	if events[0].AgentID != "agent-1" {
		t.Errorf("expected agentID='agent-1', got %q", events[0].AgentID)
	}
	if events[0].Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestNotify_Webhook(t *testing.T) {
	var received int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		var event Event
		json.NewDecoder(r.Body).Decode(&event)
		if event.Type != EventPRReadyForReview {
			t.Errorf("expected type pr_ready_for_review, got %q", event.Type)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := NewService(Config{WebhookURL: srv.URL})
	svc.Notify(EventPRReadyForReview, 10, "", "PR Ready", "A PR is ready", "http://example.com")

	// Wait for async dispatch
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&received) != 1 {
		t.Errorf("expected 1 webhook call, got %d", received)
	}
}

func TestGetEvents_FilterByType(t *testing.T) {
	svc := NewService(Config{})

	svc.Notify(EventEscalation, 1, "", "E1", "m", "")
	svc.Notify(EventPRReadyForReview, 2, "", "P1", "m", "")
	svc.Notify(EventEscalation, 3, "", "E2", "m", "")

	escalations := svc.GetEvents(EventEscalation, 10)
	if len(escalations) != 2 {
		t.Errorf("expected 2 escalation events, got %d", len(escalations))
	}

	prs := svc.GetEvents(EventPRReadyForReview, 10)
	if len(prs) != 1 {
		t.Errorf("expected 1 PR event, got %d", len(prs))
	}

	all := svc.GetEvents("", 10)
	if len(all) != 3 {
		t.Errorf("expected 3 total events, got %d", len(all))
	}
}

func TestGetEvents_Limit(t *testing.T) {
	svc := NewService(Config{})

	for i := 0; i < 10; i++ {
		svc.Notify(EventEscalation, i, "", "E", "m", "")
	}

	events := svc.GetEvents("", 3)
	if len(events) != 3 {
		t.Errorf("expected 3 events with limit, got %d", len(events))
	}
}

func TestGetEvents_ReturnsNewestFirst(t *testing.T) {
	svc := NewService(Config{})

	svc.Notify(EventEscalation, 1, "", "First", "m", "")
	svc.Notify(EventEscalation, 2, "", "Second", "m", "")

	events := svc.GetEvents("", 10)
	if events[0].Title != "Second" {
		t.Errorf("expected newest first, got %q", events[0].Title)
	}
}

func TestEventCount(t *testing.T) {
	svc := NewService(Config{})

	if svc.EventCount() != 0 {
		t.Errorf("expected 0 events initially")
	}

	svc.Notify(EventEscalation, 1, "", "E", "m", "")
	svc.Notify(EventEscalation, 2, "", "E", "m", "")

	if svc.EventCount() != 2 {
		t.Errorf("expected 2 events, got %d", svc.EventCount())
	}
}

func TestEventLog_MaxSize(t *testing.T) {
	svc := NewService(Config{})

	for i := 0; i < MaxEventLogSize+100; i++ {
		svc.Notify(EventEscalation, i, "", "E", "m", "")
	}

	if svc.EventCount() != MaxEventLogSize {
		t.Errorf("expected max %d events, got %d", MaxEventLogSize, svc.EventCount())
	}
}

func TestConvenienceMethods(t *testing.T) {
	svc := NewService(Config{})

	svc.NotifyEscalation(42, "agents disagreed")
	svc.NotifyPRReady(42, "http://pr", "Fix bug")
	svc.NotifyPlanReady(42, "http://plan-pr")
	svc.NotifyReadyForTest(42, "Login feature")
	svc.NotifyAgentError(42, "agent-1", "timeout")

	if svc.EventCount() != 5 {
		t.Errorf("expected 5 events, got %d", svc.EventCount())
	}

	events := svc.GetEvents("", 10)
	types := make(map[EventType]bool)
	for _, e := range events {
		types[e.Type] = true
	}

	expectedTypes := []EventType{EventEscalation, EventPRReadyForReview, EventPlanReady, EventReadyForTest, EventAgentError}
	for _, et := range expectedTypes {
		if !types[et] {
			t.Errorf("missing event type %q", et)
		}
	}
}

func TestWebhook_FailureDoesNotBlock(t *testing.T) {
	// Webhook to a closed server — should not block or panic
	svc := NewService(Config{WebhookURL: "http://localhost:1"})
	svc.Notify(EventEscalation, 1, "", "Test", "msg", "")

	// If we get here without hanging, the test passes
	time.Sleep(100 * time.Millisecond)
}
