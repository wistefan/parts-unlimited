// Gitea webhook handler — parses Gitea webhook payloads for pull request
// and pull request review events.  Runs on the same HTTP server as the
// Taiga webhook handler (different path).

package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// --- Gitea webhook payload structs ---

// GiteaPREvent represents a Gitea pull_request webhook payload.
type GiteaPREvent struct {
	Action      string        `json:"action"` // opened, closed, reopened, edited, synchronized, ...
	Number      int           `json:"number"`
	PullRequest GiteaPR       `json:"pull_request"`
	Repository  GiteaRepo     `json:"repository"`
	Sender      GiteaSender   `json:"sender"`
	Review      *GiteaReview  `json:"review,omitempty"` // present for pull_request_review events
}

// GiteaPR represents a pull request in a Gitea webhook payload.
type GiteaPR struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"` // open, closed
	HTMLURL   string      `json:"html_url"`
	Merged    bool        `json:"merged"`
	Mergeable bool        `json:"mergeable"`
	Head      GiteaPRRef  `json:"head"`
	Base      GiteaPRRef  `json:"base"`
	User      GiteaSender `json:"user"`
}

// GiteaPRRef represents a branch reference in a Gitea PR webhook.
type GiteaPRRef struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
}

// GiteaRepo represents a repository in a Gitea webhook payload.
type GiteaRepo struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"` // "owner/name"
	HTMLURL  string `json:"html_url"`
	Owner    GiteaSender `json:"owner"`
}

// GiteaSender represents the user who triggered the event.
type GiteaSender struct {
	ID       int    `json:"id"`
	Login    string `json:"login"`
	FullName string `json:"full_name"`
}

// GiteaReview represents a review in a pull_request_review event.
type GiteaReview struct {
	ID      int    `json:"id"`
	Body    string `json:"body"`
	State   string `json:"state"` // APPROVED, REQUEST_CHANGES, COMMENT, PENDING
	HTMLURL string `json:"html_url"`
}

// --- Gitea webhook event types ---

const (
	// GiteaEventPROpened is fired when a new PR is created.
	GiteaEventPROpened = "pr_opened"
	// GiteaEventPRMerged is fired when a PR is merged (closed + merged=true).
	GiteaEventPRMerged = "pr_merged"
	// GiteaEventPRClosed is fired when a PR is closed without merging.
	GiteaEventPRClosed = "pr_closed"
	// GiteaEventReviewRequestChanges is fired when a reviewer requests changes.
	GiteaEventReviewRequestChanges = "review_request_changes"
	// GiteaEventReviewApproved is fired when a reviewer approves.
	GiteaEventReviewApproved = "review_approved"
	// GiteaEventReviewComment is fired when a reviewer posts a comment review.
	GiteaEventReviewComment = "review_comment"
)

// GiteaEventCallback is a function that handles a classified Gitea event.
type GiteaEventCallback func(eventType string, event *GiteaPREvent) error

// GiteaHandler is the HTTP handler for Gitea webhooks.
type GiteaHandler struct {
	secret   string
	handlers map[string][]GiteaEventCallback // key: event type constant
}

// NewGiteaHandler creates a new Gitea webhook handler.
func NewGiteaHandler(secret string) *GiteaHandler {
	return &GiteaHandler{
		secret:   secret,
		handlers: make(map[string][]GiteaEventCallback),
	}
}

// On registers a callback for a specific Gitea event type.
func (h *GiteaHandler) On(eventType string, callback GiteaEventCallback) {
	h.handlers[eventType] = append(h.handlers[eventType], callback)
}

// ServeHTTP implements http.Handler.
func (h *GiteaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Verify HMAC-SHA256 signature if secret is configured
	if h.secret != "" {
		if !h.verifySignature(body, r.Header.Get("X-Gitea-Signature")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// Determine the Gitea event type from the header
	giteaEvent := r.Header.Get("X-Gitea-Event")
	if giteaEvent == "" {
		http.Error(w, "missing X-Gitea-Event header", http.StatusBadRequest)
		return
	}

	var event GiteaPREvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	log.Printf("Gitea webhook: event=%s action=%s repo=%s pr=#%d sender=%s",
		giteaEvent, event.Action, event.Repository.FullName, event.Number, event.Sender.Login)

	// Classify the event into our internal event types
	classified := classifyGiteaEvent(giteaEvent, &event)
	if classified == "" {
		// Unhandled event type — acknowledge without processing
		w.WriteHeader(http.StatusOK)
		return
	}

	// Dispatch to registered handlers
	for _, callback := range h.handlers[classified] {
		if err := callback(classified, &event); err != nil {
			log.Printf("Error handling Gitea event %s: %v", classified, err)
			http.Error(w, "handler error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// classifyGiteaEvent maps a raw Gitea webhook event to an internal event type.
func classifyGiteaEvent(giteaEvent string, event *GiteaPREvent) string {
	switch giteaEvent {
	case "pull_request":
		switch event.Action {
		case "opened":
			return GiteaEventPROpened
		case "closed":
			if event.PullRequest.Merged {
				return GiteaEventPRMerged
			}
			return GiteaEventPRClosed
		}
	case "pull_request_review":
		if event.Action == "submitted" && event.Review != nil {
			switch strings.ToUpper(event.Review.State) {
			case "REQUEST_CHANGES":
				return GiteaEventReviewRequestChanges
			case "APPROVED":
				return GiteaEventReviewApproved
			case "COMMENT":
				return GiteaEventReviewComment
			}
		}
	case "pull_request_rejected":
		// Gitea sends this event type when a reviewer requests changes.
		return GiteaEventReviewRequestChanges
	}
	return ""
}

// verifySignature checks the X-Gitea-Signature header (HMAC-SHA256).
func (h *GiteaHandler) verifySignature(body []byte, signature string) bool {
	if signature == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

// ParseTicketIDFromPRBody attempts to extract a Taiga ticket ID from the PR body.
// Looks for patterns like "Ticket #42" or "#42:" in the title/body.
func ParseTicketIDFromPRBody(title, body string) int {
	combined := title + " " + body
	// Match "Ticket #<number>" or "#<number>:" patterns
	for _, text := range []string{combined} {
		var ticketID int
		// Try "Ticket #N"
		if _, err := fmt.Sscanf(extractAfter(text, "Ticket #"), "%d", &ticketID); err == nil && ticketID > 0 {
			return ticketID
		}
		// Try "ticket-N/" in branch names embedded in the body
		if _, err := fmt.Sscanf(extractAfter(text, "ticket-"), "%d", &ticketID); err == nil && ticketID > 0 {
			return ticketID
		}
	}
	return 0
}

// extractAfter returns the substring after the first occurrence of prefix.
func extractAfter(s, prefix string) string {
	idx := strings.Index(strings.ToLower(s), strings.ToLower(prefix))
	if idx < 0 {
		return ""
	}
	return s[idx+len(prefix):]
}
