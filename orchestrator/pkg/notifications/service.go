// Package notifications implements the local notification system for the orchestrator.
// Supports webhook dispatch, an in-memory event log, and optional desktop notifications.
package notifications

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

// EventType identifies the kind of notification.
type EventType string

const (
	EventEscalation       EventType = "escalation"
	EventPRReadyForReview EventType = "pr_ready_for_review"
	EventPlanReady        EventType = "plan_ready_for_approval"
	EventReadyForTest     EventType = "ready_for_test"
	EventQuotaWarning     EventType = "quota_warning"
	EventAgentError       EventType = "agent_error"
)

// MaxEventLogSize is the maximum number of events kept in the in-memory log.
const MaxEventLogSize = 500

// Event represents a notification event.
type Event struct {
	ID        string    `json:"id"`
	Type      EventType `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	TicketID  int       `json:"ticketId,omitempty"`
	AgentID   string    `json:"agentId,omitempty"`
	Title     string    `json:"title"`
	Message   string    `json:"message"`
	URL       string    `json:"url,omitempty"`
}

// Config holds notification service configuration.
type Config struct {
	WebhookURL    string `yaml:"webhookUrl"`
	DesktopNotify bool   `yaml:"desktopNotify"`
}

// Service dispatches notifications and maintains an event log.
type Service struct {
	config     Config
	httpClient *http.Client

	mu       sync.RWMutex
	events   []Event
	nextID   int
}

// NewService creates a new notification service.
func NewService(config Config) *Service {
	return &Service{
		config: config,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		events: make([]Event, 0),
	}
}

// Notify dispatches a notification through all configured channels.
func (s *Service) Notify(eventType EventType, ticketID int, agentID, title, message, url string) {
	s.nextID++
	event := Event{
		ID:        fmt.Sprintf("evt-%d", s.nextID),
		Type:      eventType,
		Timestamp: time.Now(),
		TicketID:  ticketID,
		AgentID:   agentID,
		Title:     title,
		Message:   message,
		URL:       url,
	}

	// Store in event log
	s.mu.Lock()
	s.events = append(s.events, event)
	if len(s.events) > MaxEventLogSize {
		s.events = s.events[len(s.events)-MaxEventLogSize:]
	}
	s.mu.Unlock()

	log.Printf("Notification [%s]: %s — %s", eventType, title, message)

	// Dispatch to webhook
	if s.config.WebhookURL != "" {
		go s.dispatchWebhook(event)
	}

	// Desktop notification
	if s.config.DesktopNotify {
		go s.dispatchDesktop(event)
	}
}

// GetEvents returns the most recent events, optionally filtered by type.
func (s *Service) GetEvents(filterType EventType, limit int) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}

	var filtered []Event
	for i := len(s.events) - 1; i >= 0 && len(filtered) < limit; i-- {
		if filterType == "" || s.events[i].Type == filterType {
			filtered = append(filtered, s.events[i])
		}
	}

	return filtered
}

// EventCount returns the total number of stored events.
func (s *Service) EventCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}

// dispatchWebhook POSTs the event as JSON to the configured webhook URL.
func (s *Service) dispatchWebhook(event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("ERROR: marshaling notification: %v", err)
		return
	}

	resp, err := s.httpClient.Post(s.config.WebhookURL, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("WARNING: webhook dispatch failed: %v", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("WARNING: webhook returned status %d", resp.StatusCode)
	}
}

// dispatchDesktop sends a desktop notification via notify-send (Linux).
func (s *Service) dispatchDesktop(event Event) {
	urgency := "normal"
	if event.Type == EventEscalation || event.Type == EventAgentError {
		urgency = "critical"
	}

	cmd := exec.Command("notify-send",
		"--urgency", urgency,
		"--app-name", "dev-env",
		fmt.Sprintf("[%s] %s", event.Type, event.Title),
		event.Message,
	)

	if err := cmd.Run(); err != nil {
		log.Printf("WARNING: desktop notification failed: %v", err)
	}
}

// --- Convenience methods ---

// NotifyEscalation sends an escalation notification.
func (s *Service) NotifyEscalation(ticketID int, reason string) {
	s.Notify(EventEscalation, ticketID, "",
		fmt.Sprintf("Ticket #%d escalated", ticketID),
		reason, "")
}

// NotifyPRReady sends a PR-ready-for-review notification.
func (s *Service) NotifyPRReady(ticketID int, prURL, title string) {
	s.Notify(EventPRReadyForReview, ticketID, "",
		fmt.Sprintf("PR ready: %s", title),
		fmt.Sprintf("Ticket #%d has a PR ready for review", ticketID),
		prURL)
}

// NotifyPlanReady sends a plan-ready-for-approval notification.
func (s *Service) NotifyPlanReady(ticketID int, prURL string) {
	s.Notify(EventPlanReady, ticketID, "",
		fmt.Sprintf("Plan ready for ticket #%d", ticketID),
		"Implementation plan PR is ready for review and approval",
		prURL)
}

// NotifyReadyForTest sends a ready-for-test notification.
func (s *Service) NotifyReadyForTest(ticketID int, subject string) {
	s.Notify(EventReadyForTest, ticketID, "",
		fmt.Sprintf("Ticket #%d ready for test", ticketID),
		subject, "")
}

// NotifyAgentError sends an agent error notification.
func (s *Service) NotifyAgentError(ticketID int, agentID, errorMsg string) {
	s.Notify(EventAgentError, ticketID, agentID,
		fmt.Sprintf("Agent %s error on ticket #%d", agentID, ticketID),
		errorMsg, "")
}
