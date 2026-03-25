// Package webhooks implements the Taiga webhook receiver and event routing.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// EventHandler processes a specific type of Taiga webhook event.
type EventHandler interface {
	// HandleEvent is called when a matching webhook event is received.
	HandleEvent(event *WebhookEvent) error
}

// EventHandlerFunc is an adapter to allow use of ordinary functions as EventHandlers.
type EventHandlerFunc func(event *WebhookEvent) error

// HandleEvent calls f(event).
func (f EventHandlerFunc) HandleEvent(event *WebhookEvent) error {
	return f(event)
}

// WebhookEvent represents a parsed Taiga webhook payload.
type WebhookEvent struct {
	Action string          `json:"action"` // "create", "change", "delete", "test"
	Type   string          `json:"type"`   // "userstory", "task", "issue", etc.
	By     WebhookUser     `json:"by"`
	Date   string          `json:"date"`
	Data   json.RawMessage `json:"data"`
	Change *WebhookChange  `json:"change,omitempty"`
}

// WebhookUser identifies who triggered the event.
type WebhookUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
}

// WebhookChange holds the diff for "change" events.
type WebhookChange struct {
	Comment string          `json:"comment"`
	Diff    json.RawMessage `json:"diff"`
}

// UserStoryData represents the data field for user story events.
type UserStoryData struct {
	ID            int              `json:"id"`
	Ref           int              `json:"ref"`
	Subject       string           `json:"subject"`
	Description   string           `json:"description"`
	Status        StatusRef        `json:"status"`
	AssignedTo    *UserRef         `json:"assigned_to"`
	AssignedUsers []int            `json:"assigned_users"`
	Tags          []string         `json:"tags"`
	Project       ProjectRef       `json:"project"`
}

// StatusRef is a reference to a status in webhook payloads.
type StatusRef struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	IsClosed bool   `json:"is_closed"`
}

// UserRef is a reference to a user in webhook payloads.
type UserRef struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

// ProjectRef is a reference to a project in webhook payloads.
type ProjectRef struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// StatusChangeDiff represents the status field in a change diff.
type StatusChangeDiff struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Handler is the HTTP handler for Taiga webhooks.
type Handler struct {
	secret       string
	handlers     map[string][]EventHandler // key: "type:action" or "type:*"
	catchAll     []EventHandler
}

// NewHandler creates a new webhook handler with the given HMAC secret.
func NewHandler(secret string) *Handler {
	return &Handler{
		secret:   secret,
		handlers: make(map[string][]EventHandler),
	}
}

// On registers an event handler for a specific type and action.
// Use "*" as action to match all actions for a type.
func (h *Handler) On(eventType, action string, handler EventHandler) {
	key := eventType + ":" + action
	h.handlers[key] = append(h.handlers[key], handler)
}

// OnFunc registers a function as an event handler.
func (h *Handler) OnFunc(eventType, action string, fn func(*WebhookEvent) error) {
	h.On(eventType, action, EventHandlerFunc(fn))
}

// OnAll registers a catch-all handler for all events.
func (h *Handler) OnAll(handler EventHandler) {
	h.catchAll = append(h.catchAll, handler)
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Verify HMAC signature if secret is configured
	if h.secret != "" {
		if !h.verifySignature(body, r.Header.Get("X-Hub-Signature")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Test events get a simple 200 response
	if event.Action == "test" {
		w.WriteHeader(http.StatusOK)
		return
	}

	log.Printf("Webhook event: type=%s action=%s by=%s", event.Type, event.Action, event.By.Username)

	// Dispatch to handlers
	if err := h.dispatch(&event); err != nil {
		log.Printf("Error handling webhook event: %v", err)
		http.Error(w, "handler error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// dispatch routes an event to registered handlers.
func (h *Handler) dispatch(event *WebhookEvent) error {
	// Exact match: "type:action"
	key := event.Type + ":" + event.Action
	for _, handler := range h.handlers[key] {
		if err := handler.HandleEvent(event); err != nil {
			return fmt.Errorf("handler for %s: %w", key, err)
		}
	}

	// Wildcard match: "type:*"
	wildcardKey := event.Type + ":*"
	for _, handler := range h.handlers[wildcardKey] {
		if err := handler.HandleEvent(event); err != nil {
			return fmt.Errorf("handler for %s: %w", wildcardKey, err)
		}
	}

	// Catch-all handlers
	for _, handler := range h.catchAll {
		if err := handler.HandleEvent(event); err != nil {
			return fmt.Errorf("catch-all handler: %w", err)
		}
	}

	return nil
}

// verifySignature checks the X-Hub-Signature header against the body.
func (h *Handler) verifySignature(body []byte, signature string) bool {
	if signature == "" {
		return false
	}

	// Signature format: "sha1=<hex>"
	parts := strings.SplitN(signature, "=", 2)
	if len(parts) != 2 || parts[0] != "sha1" {
		return false
	}

	mac := hmac.New(sha1.New, []byte(h.secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(parts[1]))
}

// ParseUserStoryData extracts UserStoryData from a webhook event's raw data.
func ParseUserStoryData(event *WebhookEvent) (*UserStoryData, error) {
	var data UserStoryData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return nil, fmt.Errorf("parsing user story data: %w", err)
	}
	return &data, nil
}

// ParseStatusChange extracts the status change from a webhook change diff.
func ParseStatusChange(event *WebhookEvent) (*StatusChangeDiff, error) {
	if event.Change == nil {
		return nil, nil
	}

	var diff map[string]json.RawMessage
	if err := json.Unmarshal(event.Change.Diff, &diff); err != nil {
		return nil, fmt.Errorf("parsing change diff: %w", err)
	}

	statusRaw, ok := diff["status"]
	if !ok {
		return nil, nil
	}

	var statusChange StatusChangeDiff
	if err := json.Unmarshal(statusRaw, &statusChange); err != nil {
		return nil, fmt.Errorf("parsing status change: %w", err)
	}

	return &statusChange, nil
}
