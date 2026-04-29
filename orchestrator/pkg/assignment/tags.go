// Package assignment provides tag-based delegation helpers used by the
// orchestrator. The package previously held the FIFO queue and
// in-memory assignment maps; those have been removed in favour of
// deriving state from Taiga and Gitea on every reconcile pass.
package assignment

import (
	"fmt"
	"strings"
)

const (
	// DelegateTagPrefix is the tag prefix used to request delegation to a specialization.
	DelegateTagPrefix = "delegate:"

	// ActiveTagPrefix is the tag prefix indicating a specialization is actively working.
	ActiveTagPrefix = "active:"
)

// DelegateToSpecialization extracts the specialization from a
// "delegate:<spec>" tag. Returns "" for any other tag.
func DelegateToSpecialization(tag string) string {
	if strings.HasPrefix(tag, DelegateTagPrefix) {
		return strings.TrimPrefix(tag, DelegateTagPrefix)
	}
	return ""
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
