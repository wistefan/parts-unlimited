package reconciler

import (
	"testing"

	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
	"github.com/wistefan/dev-env/orchestrator/pkg/taiga"
)

// The human user used in assignment checks throughout these tests.
const testHumanTaigaID = 42

// comments is a small helper that builds a newest-first history slice
// from plain marker strings. Tests pass markers in the order they would
// be returned by the Taiga API (newest first), so the first argument is
// the newest comment.
func comments(cs ...string) []taiga.HistoryEntry {
	out := make([]taiga.HistoryEntry, len(cs))
	for i, c := range cs {
		out[i] = taiga.HistoryEntry{Comment: c}
	}
	return out
}

// story is a minimal UserStory constructor for tests.
func story() *taiga.UserStory {
	return &taiga.UserStory{ID: 1, Subject: "test"}
}

// storyAssignedTo returns a story with the given user ID as primary assignee.
func storyAssignedTo(userID int) *taiga.UserStory {
	s := story()
	s.AssignedTo = &userID
	return s
}

// storyWithWatcher returns a story with the given user ID in AssignedUsers.
func storyWithWatcher(userID int) *taiga.UserStory {
	s := story()
	s.AssignedUsers = []int{userID}
	return s
}

// openPR returns a PRSnapshot for an open PR with no reviews.
func openPR(number int, title string) PRSnapshot {
	return PRSnapshot{
		Owner: "claude",
		Repo:  "example",
		PR: gitea.PullRequest{
			Number: number,
			Title:  title,
			State:  prStateOpen,
		},
	}
}

// openPRWithReview returns an open PR with a single review of the given state and staleness.
func openPRWithReview(number int, reviewState string, stale bool) PRSnapshot {
	snap := openPR(number, "Step implementation")
	snap.Reviews = []gitea.PRReview{{State: reviewState, Stale: stale}}
	return snap
}

// mergedPR returns a PRSnapshot for a merged PR with the given title.
func mergedPR(number int, title string) PRSnapshot {
	return PRSnapshot{
		Owner: "claude",
		Repo:  "example",
		PR: gitea.PullRequest{
			Number: number,
			Title:  title,
			State:  "closed",
			Merged: true,
		},
	}
}

// closedPR returns a PRSnapshot for a PR closed without merging.
func closedPR(number int, title string) PRSnapshot {
	return PRSnapshot{
		Owner: "claude",
		Repo:  "example",
		PR: gitea.PullRequest{
			Number: number,
			Title:  title,
			State:  "closed",
			Merged: false,
		},
	}
}

func TestDeriveTicketState(t *testing.T) {
	tests := []struct {
		name         string
		story        *taiga.UserStory
		comments     []taiga.HistoryEntry
		prs          []PRSnapshot
		humanTaigaID int
		wantAction   Action
		wantMode     string
		wantFix      *FixTarget
	}{
		{
			name:       "nil story returns no action",
			story:      nil,
			wantAction: ActionNone,
		},
		{
			name:         "primary assignee is the human — leave alone",
			story:        storyAssignedTo(testHumanTaigaID),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "human is in AssignedUsers (watcher) — leave alone",
			story:        storyWithWatcher(testHumanTaigaID),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "humanTaigaID=0 disables the human-assignment check",
			story:        storyAssignedTo(testHumanTaigaID),
			humanTaigaID: 0,
			wantAction:   ActionSpawn,
			wantMode:     ModeAnalysis,
		},
		{
			name:         "assigned to non-human user — proceed",
			story:        storyAssignedTo(99),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeAnalysis,
		},

		// --- bare comment histories: mode derived from markers alone ---

		{
			name:         "empty comments — fresh ticket spawns analysis",
			story:        story(),
			comments:     comments(),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeAnalysis,
		},
		{
			name:         "analysis:need-info — waiting for human",
			story:        story(),
			comments:     comments("[analysis:need-info] need clarification"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "analysis:onestep-rejected — waiting for human",
			story:        story(),
			comments:     comments("[analysis:onestep-rejected] too complex"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "analysis:proceed — spawn plan",
			story:        story(),
			comments:     comments("[analysis:proceed] ready to plan"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModePlan,
		},
		{
			name:         "analysis:onestep-proceed — spawn onestep",
			story:        story(),
			comments:     comments("[analysis:onestep-proceed] tiny change"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeOnestep,
		},
		{
			name:         "step:complete — ticket finished, do nothing",
			story:        story(),
			comments:     comments("[step:complete] all steps done"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},

		// --- plan phase ---

		{
			name:         "phase:plan-created, no PRs — waiting for plan PR merge",
			story:        story(),
			comments:     comments("[phase:plan-created]", "[analysis:proceed]"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "phase:plan-created + newest fix:applied — waiting for re-review",
			story:        story(),
			comments:     comments("[fix:applied]", "[phase:plan-created]", "[analysis:proceed]"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "phase:plan-created + merged plan PR — spawn step",
			story:        story(),
			comments:     comments("[phase:plan-created]", "[analysis:proceed]"),
			prs:          []PRSnapshot{mergedPR(1, "Implementation Plan for ticket #1")},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeStep,
		},
		{
			name:         "phase:plan-created + open plan PR — waiting for merge",
			story:        story(),
			comments:     comments("[phase:plan-created]", "[analysis:proceed]"),
			prs:          []PRSnapshot{openPR(1, "Implementation Plan for ticket #1")},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},

		// --- step phase ---

		{
			name:         "step in progress, newest is step — waiting for PR",
			story:        story(),
			comments:     comments("[step:1/3] first step", "[phase:plan-created]", "[analysis:proceed]"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "step in progress, newest is fix:applied — waiting for re-review",
			story:        story(),
			comments:     comments("[fix:applied]", "[step:2/3]", "[phase:plan-created]"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name: "stale plan-created newer than step marker — resume step",
			// Artificial ordering: plan-created comment posted after step
			// work began (e.g. duplicate agent run). modeFromComments must
			// still recognise this as mid-step and resume.
			story:        story(),
			comments:     comments("[phase:plan-created]", "[step:2/5] in progress", "[analysis:proceed]"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeStep,
		},
		{
			// Recovery: step PR was merged while orchestrator was down.
			// modeFromComments says "waiting for PR" (newest marker is step),
			// fix path finds no open PR, PR-state path sees the merged step
			// PR and hands back to step mode so work continues.
			name:         "step marker + merged step PR — recover to next step",
			story:        story(),
			comments:     comments("[step:1/3]", "[phase:plan-created]"),
			prs:          []PRSnapshot{mergedPR(7, "step 1 of 3")},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeStep,
		},
		{
			name:         "step:complete wins over merged step PR",
			story:        story(),
			comments:     comments("[step:complete]", "[step:3/3]", "[phase:plan-created]"),
			prs:          []PRSnapshot{mergedPR(7, "step 3")},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},

		// --- onestep phase ---

		{
			name:         "onestep-proceed + newest fix:applied — waiting for re-review",
			story:        story(),
			comments:     comments("[fix:applied]", "[analysis:onestep-proceed]"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "onestep-proceed + step:complete — finished",
			story:        story(),
			comments:     comments("[step:complete]", "[analysis:onestep-proceed]"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},

		// --- fix detection (only fires when modeFromComments returns "") ---

		{
			name:     "step in progress + open PR with non-stale REQUEST_CHANGES — spawn fix",
			story:    story(),
			comments: comments("[step:1/3]", "[phase:plan-created]"),
			prs: []PRSnapshot{
				openPRWithReview(11, reviewStateRequestChanges, false),
			},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeFix,
			wantFix:      &FixTarget{Owner: "claude", Repo: "example", Number: 11},
		},
		{
			name:     "plan-created + open PR with non-stale REQUEST_CHANGES — spawn fix",
			story:    story(),
			comments: comments("[phase:plan-created]", "[analysis:proceed]"),
			prs: []PRSnapshot{
				openPRWithReview(12, reviewStateRequestChanges, false),
			},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeFix,
			wantFix:      &FixTarget{Owner: "claude", Repo: "example", Number: 12},
		},
		{
			name:     "stale REQUEST_CHANGES review does not trigger fix",
			story:    story(),
			comments: comments("[step:1/3]", "[phase:plan-created]"),
			prs: []PRSnapshot{
				openPRWithReview(13, reviewStateRequestChanges, true),
			},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:     "APPROVED review does not trigger fix — still waiting for merge",
			story:    story(),
			comments: comments("[step:1/3]", "[phase:plan-created]"),
			prs: []PRSnapshot{
				openPRWithReview(14, "APPROVED", false),
			},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},

		// --- PR-state recovery when comments say "waiting" ---

		{
			name:         "plan-created + merged plan PR (newest is plan-created) — recover to step",
			story:        story(),
			comments:     comments("[phase:plan-created]", "[analysis:proceed]"),
			prs:          []PRSnapshot{mergedPR(20, "Implementation Plan for ticket #1")},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeStep,
		},
		{
			name:         "plan-created + closed (unmerged) plan PR — no recovery, stays waiting",
			story:        story(),
			comments:     comments("[phase:plan-created]", "[analysis:proceed]"),
			prs:          []PRSnapshot{closedPR(21, "Implementation Plan for ticket #1")},
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},

		// --- tie-breaks inside a single comment ---

		{
			name:         "single comment carries both step:complete and fix:applied — complete wins",
			story:        story(),
			comments:     comments("[step:complete] final [fix:applied]"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveTicketState(tc.story, tc.comments, tc.prs, tc.humanTaigaID)
			if got.Action != tc.wantAction {
				t.Fatalf("Action: got %v, want %v (reason: %q)", got.Action, tc.wantAction, got.Reason)
			}
			if got.Mode != tc.wantMode {
				t.Fatalf("Mode: got %q, want %q (reason: %q)", got.Mode, tc.wantMode, got.Reason)
			}
			if (got.Fix == nil) != (tc.wantFix == nil) {
				t.Fatalf("Fix presence mismatch: got %+v, want %+v", got.Fix, tc.wantFix)
			}
			if tc.wantFix != nil && *got.Fix != *tc.wantFix {
				t.Fatalf("Fix target: got %+v, want %+v", *got.Fix, *tc.wantFix)
			}
		})
	}
}

// TestFindRepoForTicket exercises the Repo-marker regex against the
// common agent-comment formats and confirms the newest-first iteration
// picks the most recent marker.
func TestFindRepoForTicket(t *testing.T) {
	tests := []struct {
		name      string
		comments  []taiga.HistoryEntry
		wantOwner string
		wantRepo  string
	}{
		{
			name:     "no comments",
			comments: comments(),
		},
		{
			name:     "no marker",
			comments: comments("just a human note"),
		},
		{
			name:      "markdown bold with backticks",
			comments:  comments("**Repo:** `claude/example-repo`\nsome body"),
			wantOwner: "claude",
			wantRepo:  "example-repo",
		},
		{
			name:      "markdown bold without backticks",
			comments:  comments("**Repo:** claude/example-repo"),
			wantOwner: "claude",
			wantRepo:  "example-repo",
		},
		{
			name:      "plain prefix",
			comments:  comments("Repo: claude/example-repo"),
			wantOwner: "claude",
			wantRepo:  "example-repo",
		},
		{
			name: "newest marker wins when multiple agents comment",
			// newest-first: the top comment holds the authoritative repo.
			comments:  comments("**Repo:** `claude-2/forked`", "**Repo:** `claude/original`"),
			wantOwner: "claude-2",
			wantRepo:  "forked",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo := FindRepoForTicket(tc.comments)
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Errorf("FindRepoForTicket = (%q,%q), want (%q,%q)",
					owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

// TestIsPlanPR pins down the title-substring classifier used to tell
// plan PRs and step PRs apart on the recovery path.
func TestIsPlanPR(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		{"Implementation Plan for ticket #5", true},
		{"implementation plan for ticket #5", true},
		{"step 3 of 7", false},
		{"Implementation of plan step", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isPlanPR(tc.title); got != tc.want {
			t.Errorf("isPlanPR(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
}
