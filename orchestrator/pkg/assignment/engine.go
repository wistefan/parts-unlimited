// Package assignment implements the FIFO ticket assignment engine with
// concurrency control and tag-based delegation.
package assignment

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	// DelegateTagPrefix is the tag prefix used to request delegation to a specialization.
	DelegateTagPrefix = "delegate:"

	// ActiveTagPrefix is the tag prefix indicating a specialization is actively working.
	ActiveTagPrefix = "active:"
)

// TicketInfo holds the relevant information about a ticket for assignment purposes.
type TicketInfo struct {
	ID            int
	Subject       string
	StatusName    string
	Tags          []string
	AssignedUsers []int
	Version       int
}

// QueueEntry represents a ticket waiting in the FIFO queue.
type QueueEntry struct {
	TicketID int       `json:"ticketId"`
	QueuedAt time.Time `json:"queuedAt"`
}

// TicketAssignment tracks the current assignment state of a ticket.
type TicketAssignment struct {
	TicketID      int      `json:"ticketId"`
	PrimaryAgent  string   `json:"primaryAgent"`
	DelegatedTo   []string `json:"delegatedTo"`
	PlanStep      int      `json:"planStep"`
	Status        string   `json:"status"` // "queued", "assigned", "delegated", "completed"
}

// EscalationEntry tracks reassignment cycles for a ticket.
type EscalationEntry struct {
	TicketID          int `json:"ticketId"`
	ReassignmentCount int `json:"reassignmentCount"`
}

// AgentAssigner is the interface for assigning agents (fulfilled by identity.Manager).
type AgentAssigner interface {
	GetOrCreateAgent(specialization string, busyAgents map[string]bool) (AgentInfo, error)
}

// AgentInfo is a minimal interface for agent identity data needed by the engine.
type AgentInfo struct {
	ID             string
	Specialization string
	TaigaUserID    int
}

// TicketUpdater is the interface for updating tickets in Taiga.
type TicketUpdater interface {
	UpdateTicketStatus(ticketID, statusID, version int) error
	UpdateTicketAssignment(ticketID int, assignedUsers []int, version int) error
	UpdateTicketTags(ticketID int, tags [][]string, version int) error
	AddTicketComment(ticketID int, comment string, version int) error
}

// Engine is the ticket assignment engine.
type Engine struct {
	maxConcurrency      int
	escalationThreshold int

	mu          sync.Mutex
	queue       []QueueEntry
	assignments map[int]*TicketAssignment // keyed by ticket ID
	escalations map[int]*EscalationEntry  // keyed by ticket ID
	busyAgents  map[string]int            // agent ID -> ticket ID
}

// NewEngine creates a new assignment engine.
func NewEngine(maxConcurrency, escalationThreshold int) *Engine {
	return &Engine{
		maxConcurrency:      maxConcurrency,
		escalationThreshold: escalationThreshold,
		queue:               make([]QueueEntry, 0),
		assignments:         make(map[int]*TicketAssignment),
		escalations:         make(map[int]*EscalationEntry),
		busyAgents:          make(map[string]int),
	}
}

// Enqueue adds a ticket to the FIFO queue if not already present.
func (e *Engine) Enqueue(ticketID int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Check if already queued or assigned
	for _, entry := range e.queue {
		if entry.TicketID == ticketID {
			log.Printf("Ticket %d already in queue", ticketID)
			return
		}
	}
	if _, exists := e.assignments[ticketID]; exists {
		log.Printf("Ticket %d already assigned", ticketID)
		return
	}

	e.queue = append(e.queue, QueueEntry{
		TicketID: ticketID,
		QueuedAt: time.Now(),
	})
	log.Printf("Ticket %d enqueued (queue length: %d)", ticketID, len(e.queue))
}

// Dequeue returns the next ticket from the queue, or nil if the queue is empty
// or max concurrency is reached.
func (e *Engine) Dequeue() *QueueEntry {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.queue) == 0 {
		return nil
	}

	if len(e.busyAgents) >= e.maxConcurrency {
		log.Printf("Max concurrency reached (%d/%d), not dequeuing", len(e.busyAgents), e.maxConcurrency)
		return nil
	}

	entry := e.queue[0]
	e.queue = e.queue[1:]
	return &entry
}

// AssignAgent records that an agent is working on a ticket.
func (e *Engine) AssignAgent(ticketID int, agentID string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.assignments[ticketID] = &TicketAssignment{
		TicketID:     ticketID,
		PrimaryAgent: agentID,
		Status:       "assigned",
	}
	e.busyAgents[agentID] = ticketID

	log.Printf("Agent %s assigned to ticket %d", agentID, ticketID)
}

// DelegateToSpecialization processes a delegation tag on a ticket.
// Returns the specialization extracted from the tag, or empty string if not a delegation tag.
func (e *Engine) DelegateToSpecialization(tag string) string {
	if strings.HasPrefix(tag, DelegateTagPrefix) {
		return strings.TrimPrefix(tag, DelegateTagPrefix)
	}
	return ""
}

// RecordDelegation records that a specialized agent is working on a ticket.
func (e *Engine) RecordDelegation(ticketID int, agentID string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	assignment, exists := e.assignments[ticketID]
	if !exists {
		assignment = &TicketAssignment{
			TicketID: ticketID,
			Status:   "delegated",
		}
		e.assignments[ticketID] = assignment
	}

	assignment.DelegatedTo = append(assignment.DelegatedTo, agentID)
	assignment.Status = "delegated"
	e.busyAgents[agentID] = ticketID

	log.Printf("Ticket %d delegated to %s (total delegations: %d)",
		ticketID, agentID, len(assignment.DelegatedTo))
}

// CompleteDelegation records that a specialized agent finished its delegation.
// Returns true if all delegations are complete (general-purpose agent can resume).
func (e *Engine) CompleteDelegation(ticketID int, agentID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.busyAgents, agentID)

	assignment, exists := e.assignments[ticketID]
	if !exists {
		return true
	}

	// Remove the agent from delegated list
	filtered := make([]string, 0, len(assignment.DelegatedTo))
	for _, id := range assignment.DelegatedTo {
		if id != agentID {
			filtered = append(filtered, id)
		}
	}
	assignment.DelegatedTo = filtered

	allDone := len(assignment.DelegatedTo) == 0
	if allDone {
		assignment.Status = "assigned"
		log.Printf("All delegations complete for ticket %d, primary agent can resume", ticketID)
	} else {
		log.Printf("Delegation from %s complete for ticket %d, %d remaining",
			agentID, ticketID, len(assignment.DelegatedTo))
	}

	return allDone
}

// CompleteTicket marks a ticket as completed and frees the agent.
func (e *Engine) CompleteTicket(ticketID int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	assignment, exists := e.assignments[ticketID]
	if exists {
		delete(e.busyAgents, assignment.PrimaryAgent)
		for _, delegated := range assignment.DelegatedTo {
			delete(e.busyAgents, delegated)
		}
		assignment.Status = "completed"
	}
	delete(e.assignments, ticketID)
	delete(e.escalations, ticketID)

	log.Printf("Ticket %d completed", ticketID)
}

// RecordReassignment increments the no-op reassignment counter for a ticket.
// Returns true if the escalation threshold is reached.
func (e *Engine) RecordReassignment(ticketID int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	entry, exists := e.escalations[ticketID]
	if !exists {
		entry = &EscalationEntry{TicketID: ticketID}
		e.escalations[ticketID] = entry
	}

	entry.ReassignmentCount++
	escalate := entry.ReassignmentCount >= e.escalationThreshold

	if escalate {
		log.Printf("Ticket %d escalation threshold reached (%d reassignments)",
			ticketID, entry.ReassignmentCount)
	}

	return escalate
}

// ResetEscalation resets the escalation counter for a ticket (called when actual work is done).
func (e *Engine) ResetEscalation(ticketID int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.escalations, ticketID)
}

// GetBusyAgents returns a map of currently busy agent IDs.
func (e *Engine) GetBusyAgents() map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := make(map[string]bool, len(e.busyAgents))
	for id := range e.busyAgents {
		result[id] = true
	}
	return result
}

// GetAssignment returns the current assignment for a ticket, or nil.
func (e *Engine) GetAssignment(ticketID int) *TicketAssignment {
	e.mu.Lock()
	defer e.mu.Unlock()

	if a, ok := e.assignments[ticketID]; ok {
		// Return a copy
		copy := *a
		copy.DelegatedTo = append([]string{}, a.DelegatedTo...)
		return &copy
	}
	return nil
}

// QueueLength returns the number of tickets waiting in the queue.
func (e *Engine) QueueLength() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.queue)
}

// ActiveCount returns the number of currently active agent assignments.
func (e *Engine) ActiveCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.busyAgents)
}

// GetQueue returns a copy of the current queue.
func (e *Engine) GetQueue() []QueueEntry {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := make([]QueueEntry, len(e.queue))
	copy(result, e.queue)
	return result
}

// GetAllAssignments returns a copy of all current assignments.
func (e *Engine) GetAllAssignments() map[int]*TicketAssignment {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := make(map[int]*TicketAssignment, len(e.assignments))
	for k, v := range e.assignments {
		copy := *v
		copy.DelegatedTo = append([]string{}, v.DelegatedTo...)
		result[k] = &copy
	}
	return result
}

// ExtractDelegationTags returns all delegation tags from a tag list.
func ExtractDelegationTags(tags []string) []string {
	var result []string
	for _, tag := range tags {
		if strings.HasPrefix(tag, DelegateTagPrefix) {
			result = append(result, tag)
		}
	}
	return result
}

// ExtractActiveTags returns all active delegation tags from a tag list.
func ExtractActiveTags(tags []string) []string {
	var result []string
	for _, tag := range tags {
		if strings.HasPrefix(tag, ActiveTagPrefix) {
			result = append(result, tag)
		}
	}
	return result
}

// ReplaceDelegateWithActive replaces a delegate: tag with an active: tag in a tag list.
func ReplaceDelegateWithActive(tags []string, specialization string) []string {
	delegateTag := DelegateTagPrefix + specialization
	activeTag := ActiveTagPrefix + specialization

	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag == delegateTag {
			result = append(result, activeTag)
		} else {
			result = append(result, tag)
		}
	}
	return result
}

// RemoveActiveTag removes an active: tag from a tag list.
func RemoveActiveTag(tags []string, specialization string) []string {
	activeTag := ActiveTagPrefix + specialization

	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag != activeTag {
			result = append(result, tag)
		}
	}
	return result
}

// HasActiveDelegations checks if a tag list contains any active: tags.
func HasActiveDelegations(tags []string) bool {
	for _, tag := range tags {
		if strings.HasPrefix(tag, ActiveTagPrefix) {
			return true
		}
	}
	return false
}

// FormatEscalationComment generates the comment text for escalating a ticket to a human.
func FormatEscalationComment(ticketID int, reason string) string {
	return fmt.Sprintf(
		"**Escalation**: This ticket has been escalated to a human.\n\n"+
			"**Reason:** %s\n\n"+
			"Agents have been unable to resolve this independently after multiple reassignment attempts. "+
			"Please provide guidance or reassign manually.",
		reason,
	)
}
