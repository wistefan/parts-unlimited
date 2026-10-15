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
		// reviewEnabled defaults to false so these cases exercise the
		// lifecycle decision tree in isolation; the review rule has its
		// own table in TestDeriveTicketStateReview.
		reviewEnabled bool
		wantAction    Action
		wantMode      string
		wantFix       *FixTarget
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
			name:         "analysis:need-info + human reply — re-run analysis",
			story:        story(),
			comments:     comments("Here is the additional info you asked for", "[analysis:need-info] need clarification"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModeAnalysis,
		},
		{
			name:         "analysis:need-info + second need-info after reply — still waiting",
			story:        story(),
			comments:     comments("[analysis:need-info] still unclear", "extra details", "[analysis:need-info] need clarification"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionNone,
		},
		{
			name:         "analysis:need-info + marker reply — not treated as human reply",
			story:        story(),
			comments:     comments("[analysis:proceed] approved", "[analysis:need-info] need clarification"),
			humanTaigaID: testHumanTaigaID,
			wantAction:   ActionSpawn,
			wantMode:     ModePlan,
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
			got := DeriveTicketState(tc.story, tc.comments, tc.prs, tc.humanTaigaID, tc.reviewEnabled)
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
			name: "oldest marker wins when multiple agents comment",
			// Comments are newest-first (the order GetComments returns),
			// so the *last* element here is the oldest. The original
			// claim sticks; later comments cannot rebind the canonical
			// fork — see the godoc on FindRepoForTicket.
			comments:  comments("**Repo:** `claude-2/forked`", "**Repo:** `claude/original`"),
			wantOwner: "claude",
			wantRepo:  "original",
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

// --- automated PR review ---

// Fixed SHAs used across the review tests. headSHA is the revision a PR
// currently points at; oldSHA is a revision it pointed at before new
// commits were pushed.
const (
	headSHA = "0a1b2c3d4e5f60718293a4b5c6d7e8f901234567"
	oldSHA  = "fedcba98765432100123456789abcdefabcdef01"
)

// openPRAtSHA returns an open PR whose head points at the given commit.
func openPRAtSHA(number int, title, sha string) PRSnapshot {
	snap := openPR(number, title)
	snap.PR.Head.Sha = sha
	return snap
}

// agentReview builds a review as a review agent would have posted it:
// COMMENT event, login carrying [ReviewAgentPrefix], and the reviewed
// revision recorded in the body marker.
func agentReview(commitID, bodySHA string) gitea.PRReview {
	r := gitea.PRReview{State: "COMMENT", CommitID: commitID}
	r.User.Login = ReviewAgentPrefix + "1"
	if bodySHA != "" {
		r.Body = "Looks reasonable overall.\n\n" + ReviewSHAMarker(bodySHA)
	}
	return r
}

// humanReview builds a review posted by a person.
func humanReview(state, commitID string) gitea.PRReview {
	r := gitea.PRReview{State: state, CommitID: commitID}
	r.User.Login = "wistefan"
	return r
}

func TestDeriveTicketStateReview(t *testing.T) {
	// Every case below sits in the "waiting for a PR event" window: the
	// ticket has a [step:2/5] marker and an open PR, so no other mode is
	// due. That isolates the review rule from the rest of the tree.
	waiting := comments("[step:2/5] shipped step 2")

	tests := []struct {
		name string
		// comments overrides the default `waiting` history for cases
		// that need a different lifecycle position.
		comments      []taiga.HistoryEntry
		prs           []PRSnapshot
		reviewEnabled bool
		wantAction    Action
		wantReview    *ReviewTarget
	}{
		{
			name:          "open PR with no reviews at all — review it",
			prs:           []PRSnapshot{openPRAtSHA(7, "Step implementation", headSHA)},
			reviewEnabled: true,
			wantAction:    ActionSpawn,
			wantReview:    &ReviewTarget{Owner: "claude", Repo: "example", Number: 7, HeadSHA: headSHA},
		},
		{
			name: "agent review recorded via commit_id at head — skip",
			prs: func() []PRSnapshot {
				s := openPRAtSHA(7, "Step implementation", headSHA)
				s.Reviews = []gitea.PRReview{agentReview(headSHA, "")}
				return []PRSnapshot{s}
			}(),
			reviewEnabled: true,
			wantAction:    ActionNone,
		},
		{
			name: "agent review recorded via body marker at head — skip",
			prs: func() []PRSnapshot {
				s := openPRAtSHA(7, "Step implementation", headSHA)
				// commit_id absent, as Gitea leaves it on some COMMENT reviews.
				s.Reviews = []gitea.PRReview{agentReview("", headSHA)}
				return []PRSnapshot{s}
			}(),
			reviewEnabled: true,
			wantAction:    ActionNone,
		},
		{
			name: "agent review exists but only for an older revision — re-review",
			prs: func() []PRSnapshot {
				s := openPRAtSHA(7, "Step implementation", headSHA)
				s.Reviews = []gitea.PRReview{agentReview(oldSHA, oldSHA)}
				return []PRSnapshot{s}
			}(),
			reviewEnabled: true,
			wantAction:    ActionSpawn,
			wantReview:    &ReviewTarget{Owner: "claude", Repo: "example", Number: 7, HeadSHA: headSHA},
		},
		{
			name: "only a human has reviewed the head revision — still review",
			prs: func() []PRSnapshot {
				s := openPRAtSHA(7, "Step implementation", headSHA)
				s.Reviews = []gitea.PRReview{humanReview("APPROVED", headSHA)}
				return []PRSnapshot{s}
			}(),
			reviewEnabled: true,
			wantAction:    ActionSpawn,
			wantReview:    &ReviewTarget{Owner: "claude", Repo: "example", Number: 7, HeadSHA: headSHA},
		},
		{
			name:          "plan PRs are reviewed too",
			prs:           []PRSnapshot{openPRAtSHA(3, "Ticket #1: Implementation Plan", headSHA)},
			reviewEnabled: true,
			wantAction:    ActionSpawn,
			wantReview:    &ReviewTarget{Owner: "claude", Repo: "example", Number: 3, HeadSHA: headSHA},
		},
		{
			name:          "unknown head SHA — skip rather than re-review every pass",
			prs:           []PRSnapshot{openPRAtSHA(7, "Step implementation", "")},
			reviewEnabled: true,
			wantAction:    ActionNone,
		},
		{
			// [step:complete] parks the ticket, so the merged PR is the
			// only thing left that could conceivably be reviewed.
			name:          "merged PRs are never reviewed",
			comments:      comments("[step:complete] all steps done"),
			prs:           []PRSnapshot{mergedPR(7, "Step implementation")},
			reviewEnabled: true,
			wantAction:    ActionNone,
		},
		{
			name:          "closed-unmerged PRs are never reviewed",
			prs:           []PRSnapshot{closedPR(7, "Step implementation")},
			reviewEnabled: true,
			wantAction:    ActionNone,
		},
		{
			name:          "review disabled — never spawn a reviewer",
			prs:           []PRSnapshot{openPRAtSHA(7, "Step implementation", headSHA)},
			reviewEnabled: false,
			wantAction:    ActionNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			history := tc.comments
			if history == nil {
				history = waiting
			}
			got := DeriveTicketState(story(), history, tc.prs, testHumanTaigaID, tc.reviewEnabled)
			if got.Action != tc.wantAction {
				t.Fatalf("Action: got %v, want %v (reason: %q)", got.Action, tc.wantAction, got.Reason)
			}
			if tc.wantAction == ActionSpawn && got.Mode != ModeReview {
				t.Fatalf("Mode: got %q, want %q", got.Mode, ModeReview)
			}
			if (got.Review == nil) != (tc.wantReview == nil) {
				t.Fatalf("Review presence mismatch: got %+v, want %+v", got.Review, tc.wantReview)
			}
			if tc.wantReview != nil && *got.Review != *tc.wantReview {
				t.Fatalf("Review target: got %+v, want %+v", *got.Review, *tc.wantReview)
			}
		})
	}
}

// TestReviewNeverPreemptsWork pins the ordering guarantee: a review is
// only ever decided when nothing else is due. If this regresses, agent
// work silently stalls behind reviews.
func TestReviewNeverPreemptsWork(t *testing.T) {
	reviewable := openPRAtSHA(7, "Step implementation", headSHA)

	tests := []struct {
		name     string
		comments []taiga.HistoryEntry
		prs      []PRSnapshot
		wantMode string
	}{
		{
			name:     "analysis is due — analysis wins over review",
			comments: nil,
			prs:      []PRSnapshot{reviewable},
			wantMode: ModeAnalysis,
		},
		{
			name:     "analysis approved, plan is due — plan wins over review",
			comments: comments("[analysis:proceed]"),
			prs:      []PRSnapshot{reviewable},
			wantMode: ModePlan,
		},
		{
			name:     "human requested changes — fix wins over review",
			comments: comments("[step:2/5] shipped step 2"),
			prs: func() []PRSnapshot {
				s := openPRAtSHA(7, "Step implementation", headSHA)
				s.Reviews = []gitea.PRReview{humanReview(reviewStateRequestChanges, headSHA)}
				return []PRSnapshot{s}
			}(),
			wantMode: ModeFix,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveTicketState(story(), tc.comments, tc.prs, testHumanTaigaID, true)
			if got.Action != ActionSpawn {
				t.Fatalf("Action: got %v, want ActionSpawn (reason: %q)", got.Action, got.Reason)
			}
			if got.Mode != tc.wantMode {
				t.Fatalf("Mode: got %q, want %q (reason: %q)", got.Mode, tc.wantMode, got.Reason)
			}
		})
	}
}

// TestReviewCapStopsRunawayReviews covers the failure mode the SHA check
// cannot: a review that never lands (bad access, rejected payload, Job
// killed) leaves no marker, so the PR looks unreviewed on every single
// pass. The cap is what keeps that from spawning a reviewer forever.
func TestReviewCapStopsRunawayReviews(t *testing.T) {
	waiting := comments("[step:2/5] shipped step 2")

	// Reviews that all landed on older revisions — exactly what an
	// unposted-review loop looks like from Gitea's side.
	withAgentReviews := func(n int) []PRSnapshot {
		s := openPRAtSHA(7, "Step implementation", headSHA)
		for i := 0; i < n; i++ {
			s.Reviews = append(s.Reviews, agentReview(oldSHA, oldSHA))
		}
		return []PRSnapshot{s}
	}

	tests := []struct {
		name         string
		agentReviews int
		wantAction   Action
	}{
		{name: "well under the cap", agentReviews: 1, wantAction: ActionSpawn},
		{name: "one below the cap", agentReviews: maxAgentReviewsPerPR - 1, wantAction: ActionSpawn},
		{name: "at the cap", agentReviews: maxAgentReviewsPerPR, wantAction: ActionNone},
		{name: "past the cap", agentReviews: maxAgentReviewsPerPR + 5, wantAction: ActionNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveTicketState(story(), waiting, withAgentReviews(tc.agentReviews), testHumanTaigaID, true)
			if got.Action != tc.wantAction {
				t.Fatalf("Action: got %v, want %v (reason: %q)", got.Action, tc.wantAction, got.Reason)
			}
		})
	}
}

// TestReviewCapIgnoresHumanReviews makes sure a heavily human-reviewed
// PR is not starved of automated review by the cap.
func TestReviewCapIgnoresHumanReviews(t *testing.T) {
	s := openPRAtSHA(7, "Step implementation", headSHA)
	for i := 0; i < maxAgentReviewsPerPR*2; i++ {
		s.Reviews = append(s.Reviews, humanReview("COMMENT", oldSHA))
	}

	got := DeriveTicketState(story(), comments("[step:2/5] shipped"), []PRSnapshot{s}, testHumanTaigaID, true)
	if got.Action != ActionSpawn || got.Mode != ModeReview {
		t.Fatalf("got %v/%q, want ActionSpawn/%s (reason: %q)", got.Action, got.Mode, ModeReview, got.Reason)
	}
}
