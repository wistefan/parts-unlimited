package assignment

import (
	"testing"
)

func TestDelegateToSpecialization(t *testing.T) {
	tests := []struct {
		tag    string
		expect string
	}{
		{"delegate:frontend", "frontend"},
		{"delegate:test", "test"},
		{"delegate:backend", "backend"},
		{"active:frontend", ""},
		{"other-tag", ""},
		{"delegate:", ""},
	}

	for _, tt := range tests {
		result := DelegateToSpecialization(tt.tag)
		if result != tt.expect {
			t.Errorf("DelegateToSpecialization(%q): expected %q, got %q", tt.tag, tt.expect, result)
		}
	}
}

func TestExtractDelegationTags(t *testing.T) {
	tags := []string{"frontend", "delegate:test", "delegate:documentation", "active:backend"}
	result := ExtractDelegationTags(tags)

	if len(result) != 2 {
		t.Errorf("expected 2 delegation tags, got %d", len(result))
	}
}

func TestExtractActiveTags(t *testing.T) {
	tags := []string{"frontend", "delegate:test", "active:backend", "active:frontend"}
	result := ExtractActiveTags(tags)

	if len(result) != 2 {
		t.Errorf("expected 2 active tags, got %d", len(result))
	}
}

func TestReplaceDelegateWithActive(t *testing.T) {
	tags := []string{"frontend", "delegate:test", "other"}
	result := ReplaceDelegateWithActive(tags, "test")

	expected := []string{"frontend", "active:test", "other"}
	if len(result) != len(expected) {
		t.Fatalf("expected %d tags, got %d", len(expected), len(result))
	}
	for i, tag := range result {
		if tag != expected[i] {
			t.Errorf("tag[%d]: expected %q, got %q", i, expected[i], tag)
		}
	}
}

func TestRemoveActiveTag(t *testing.T) {
	tags := []string{"frontend", "active:test", "active:backend", "other"}
	result := RemoveActiveTag(tags, "test")

	if len(result) != 3 {
		t.Errorf("expected 3 tags after removal, got %d", len(result))
	}
	for _, tag := range result {
		if tag == "active:test" {
			t.Error("active:test should have been removed")
		}
	}
}

func TestHasActiveDelegations(t *testing.T) {
	if HasActiveDelegations([]string{"frontend", "delegate:test"}) {
		t.Error("should not have active delegations")
	}
	if !HasActiveDelegations([]string{"frontend", "active:backend"}) {
		t.Error("should have active delegations")
	}
	if HasActiveDelegations([]string{}) {
		t.Error("empty tags should not have active delegations")
	}
}

func TestFormatEscalationComment(t *testing.T) {
	comment := FormatEscalationComment(42, "agents disagreed on approach")
	if comment == "" {
		t.Error("expected non-empty comment")
	}
	if !containsString(comment, "Escalation") {
		t.Error("expected comment to contain 'Escalation'")
	}
	if !containsString(comment, "agents disagreed on approach") {
		t.Error("expected comment to contain the reason")
	}
}

func containsString(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 && contains(haystack, needle)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
