// Package reconciler derives the orchestrator's per-ticket decisions from
// external state alone (Taiga ticket fields + comment markers, Gitea PR
// state + reviews). It holds no persisted state of its own.
//
// The package's centerpiece is [DeriveTicketState], a pure function that
// returns the single action the orchestrator should take for a ticket on
// a given pass. Callers (the reconcile loop, webhook handlers) supply the
// already-fetched external state, invoke the function, and then execute
// the returned [Decision] — spawn an agent in the given mode, or do
// nothing while the ticket waits for a human/PR event.
//
// Keeping the decision logic pure makes it straightforward to table-test
// every lifecycle marker combination without mocking Kubernetes, Taiga,
// or Gitea clients.
package reconciler

import (
	"regexp"
	"strings"

	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
	"github.com/wistefan/dev-env/orchestrator/pkg/taiga"
)

// repoMarkerRE parses the repo line every agent embeds in its Taiga
// comment. Tolerates the common variants agents have emitted:
//
//	**Repo:** `owner/name`
//	**Repo:** owner/name
//	Repo: owner/name
//
// Identical to the regex in cmd/orchestrator/main.go; kept here so the
// reconciler package is self-contained and does not depend on orchestrator
// internals.
var repoMarkerRE = regexp.MustCompile("(?:\\*\\*)?Repo:(?:\\*\\*)?[\\s`]*([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)")

// FindRepoForTicket returns the canonical Gitea owner/name for a ticket
// by scanning its Taiga comment history for a `**Repo:** owner/name`
// marker. Returns ("", "") when no marker exists yet (first-agent case);
// the caller should fall back to other resolution paths.
//
// The input must be newest-first (the order GetComments returns); we
// iterate in reverse so the *oldest* agent comment carrying a Repo:
// marker wins. Anchoring on the first claim is what keeps the canonical
// fork pointer stable when a stray agent (different identity / fork)
// later comments — see the godoc on cmd/orchestrator.findRepoForTicket
// for the failure mode this defends against.
func FindRepoForTicket(comments []taiga.HistoryEntry) (owner, name string) {
	for i := len(comments) - 1; i >= 0; i-- {
		if m := repoMarkerRE.FindStringSubmatch(comments[i].Comment); len(m) == 3 {
			return m[1], m[2]
		}
	}
	return "", ""
}

// Agent mode identifiers. These are the values the orchestrator passes to
// the agent container as the $MODE environment variable via the Kubernetes
// Job spec, and the suffix used when naming the Job itself.
const (
	ModeAnalysis = "analysis"
	ModePlan     = "plan"
	ModeStep     = "step"
	ModeOnestep  = "onestep"
	ModeFix      = "fix"
	ModeReview   = "review"
)

// ReviewAgentPrefix is the agent-ID prefix of the dedicated reviewer
// specialization (see identity.DefaultSpecializations). Reviews posted
// by a user whose login carries this prefix are the orchestrator's own
// automated reviews; everything else is a human (or a working agent's
// PR conversation) and must not suppress a review.
const ReviewAgentPrefix = "review-agent-"

// reviewSHAMarkerPrefix opens the HTML comment every automated review
// embeds in its body to record the exact revision it examined:
//
//	<!-- agent-review: 0a1b2c3... -->
//
// Gitea populates a review's commit_id inconsistently for COMMENT-event
// reviews, so the marker is the authoritative record of "this revision
// has been reviewed". agent/bootstrap.sh writes it; [ReviewSHAMarker]
// is the single definition of the format shared by both sides.
const (
	reviewSHAMarkerPrefix = "<!-- agent-review: "
	reviewSHAMarkerSuffix = " -->"
)

// ReviewSHAMarker returns the marker an automated review embeds in its
// body to record the head revision it reviewed. Keep in sync with the
// REVIEW_SHA_MARKER construction in agent/bootstrap.sh.
func ReviewSHAMarker(sha string) string {
	return reviewSHAMarkerPrefix + sha + reviewSHAMarkerSuffix
}

// Lifecycle marker strings posted by agents in Taiga comments. The
// reconciler recognises these via substring match so agents may embed them
// inside richer comments (e.g. `[step:3/7]` or `[pr-rejected:42]`).
const (
	markerStepComplete          = "[step:complete]"
	markerStepPrefix            = "[step:"
	markerFixApplied            = "[fix:applied]"
	markerPhasePlanCreated      = "[phase:plan-created]"
	markerAnalysisOneStepOK     = "[analysis:onestep-proceed]"
	markerAnalysisOneStepReject = "[analysis:onestep-rejected]"
	markerAnalysisProceed       = "[analysis:proceed]"
	markerAnalysisNeedInfo      = "[analysis:need-info]"
)

// PR title substrings used to classify merged PRs as plan vs. step PRs.
// Plan PRs open the implementation-plan document and ship the AGENTS.md
// codebase context; step PRs implement a numbered plan step.
const (
	planPRTitleMarker      = "Implementation Plan"
	planPRTitleMarkerLower = "implementation plan"
)

// PR review states used by Gitea's reviews API.
const (
	reviewStateRequestChanges = "REQUEST_CHANGES"
	prStateOpen               = "open"
)

// Action is the decision the reconciler returns for a given ticket on a
// given pass.
type Action int

const (
	// ActionNone indicates the orchestrator should not spawn an agent for
	// this ticket on this pass. The ticket is either complete, waiting for
	// a human, waiting for a PR event, or assigned to a human directly.
	ActionNone Action = iota

	// ActionSpawn indicates the orchestrator should spawn an agent in the
	// [Decision.Mode] for this ticket. When Mode == ModeFix, [Decision.Fix]
	// identifies the PR whose review comments need to be addressed.
	ActionSpawn
)

// FixTarget identifies the pull request a fix agent should attend to.
type FixTarget struct {
	Owner  string
	Repo   string
	Number int
}

// ReviewTarget identifies the pull request a review agent should
// examine, pinned to the exact head revision the decision was made
// against. Carrying the SHA through to the agent means the review the
// agent posts records the revision the reconciler judged unreviewed,
// even if someone pushes again while the Job is starting.
type ReviewTarget struct {
	Owner   string
	Repo    string
	Number  int
	HeadSHA string
}

// Decision is the reconciler's verdict for a ticket on a single pass.
// Callers inspect [Decision.Action] first and, when it is ActionSpawn,
// dispatch based on [Decision.Mode].
type Decision struct {
	// Action is what the orchestrator should do.
	Action Action

	// Mode is the agent mode to spawn when Action == ActionSpawn. Empty
	// otherwise.
	Mode string

	// Fix identifies the PR needing review follow-up. Non-nil only when
	// Mode == ModeFix.
	Fix *FixTarget

	// Review identifies the PR awaiting an automated senior-engineer
	// review. Non-nil only when Mode == ModeReview.
	Review *ReviewTarget

	// Reason is a short human-readable explanation of why the reconciler
	// chose this action. Intended for logs, not control flow.
	Reason string
}

// PRSnapshot bundles a pull request with the reviews on it, plus the
// repository it lives in. The reconciler needs reviews to detect fix
// targets (open PRs with non-stale REQUEST_CHANGES reviews) without
// issuing a second API call inside the pure function.
type PRSnapshot struct {
	Owner   string
	Repo    string
	PR      gitea.PullRequest
	Reviews []gitea.PRReview
}

// DeriveTicketState decides what action the orchestrator should take for
// a ticket given its current external state. The function is pure: the
// same inputs always produce the same [Decision].
//
// Inputs:
//   - story: the Taiga user story (status, assignees, tags).
//   - comments: the ticket's comment history, newest-first (the order
//     Taiga's history endpoint returns).
//   - prs: all pull requests associated with the ticket, across all its
//     repositories, each with its own review list.
//   - humanTaigaID: the Taiga user ID of the human overseeing the
//     orchestrator. Pass 0 to disable the "assigned to human" check.
//   - reviewEnabled: whether automated senior-engineer PR reviews are
//     turned on. Pass false to suppress ModeReview entirely.
//
// The decision logic is the single source of truth for "what mode
// (if any) should run for this ticket right now": comment markers
// drive most transitions, with PR state used to recover when a PR was
// merged or had changes requested while the orchestrator was down.
func DeriveTicketState(
	story *taiga.UserStory,
	comments []taiga.HistoryEntry,
	prs []PRSnapshot,
	humanTaigaID int,
	reviewEnabled bool,
) Decision {
	if story == nil {
		return Decision{Action: ActionNone, Reason: "nil story"}
	}

	if isAssignedToHuman(story, humanTaigaID) {
		return Decision{Action: ActionNone, Reason: "ticket assigned to human"}
	}

	mode, modeReason := modeFromComments(comments)
	if mode != "" {
		return Decision{Action: ActionSpawn, Mode: mode, Reason: modeReason}
	}

	if fix := findFixTarget(prs); fix != nil {
		return Decision{
			Action: ActionSpawn,
			Mode:   ModeFix,
			Fix:    fix,
			Reason: "open PR has non-stale REQUEST_CHANGES review",
		}
	}

	if recovered, reason := modeFromPRState(prs, comments); recovered != "" {
		return Decision{Action: ActionSpawn, Mode: recovered, Reason: reason}
	}

	// Nothing else to do for this ticket: no agent work is pending and
	// no PR needs fixing. This is exactly the window in which a PR sits
	// open awaiting a human — so use it to post an automated review, if
	// one is not already on record for the PR's current head revision.
	//
	// Deliberately last: a review must never displace plan/step/fix
	// work, and the reviewer runs only against revisions that are
	// otherwise idle.
	if reviewEnabled {
		if rev := findReviewTarget(prs); rev != nil {
			return Decision{
				Action: ActionSpawn,
				Mode:   ModeReview,
				Review: rev,
				Reason: "open PR has no automated review for its current head revision",
			}
		}
	}

	return Decision{Action: ActionNone, Reason: modeReason}
}

// isAssignedToHuman checks both AssignedTo (primary assignee) and
// AssignedUsers (watchers). Matches the behaviour of the existing
// ticketAssignedToHuman helper in cmd/orchestrator/main.go rather than
// the looser check in the in-progress reconcile loop — the stricter
// form is the safer one for "leave this ticket alone".
func isAssignedToHuman(story *taiga.UserStory, humanTaigaID int) bool {
	if humanTaigaID == 0 {
		return false
	}
	if story.AssignedTo != nil && *story.AssignedTo == humanTaigaID {
		return true
	}
	for _, uid := range story.AssignedUsers {
		if uid == humanTaigaID {
			return true
		}
	}
	return false
}

// modeFromComments derives the next agent mode from a ticket's
// comment history by scanning lifecycle markers. The second return
// value is a short reason intended for logs when the mode is empty
// ("" means "waiting").
func modeFromComments(comments []taiga.HistoryEntry) (string, string) {
	hasAnalysisProceed := false
	hasAnalysisNeedInfo := false
	hasAnalysisOneStepProceed := false
	hasAnalysisOneStepRejected := false
	hasPlanCreated := false
	hasStep := false
	hasStepComplete := false

	// Comments arrive newest-first; record the newest marker of any
	// recognised kind so we can tell "currently waiting for X" apart
	// from "saw X earlier, moved on".
	newestMarker := ""
	for _, entry := range comments {
		c := entry.Comment
		if strings.Contains(c, markerStepComplete) {
			hasStepComplete = true
			if newestMarker == "" {
				newestMarker = "step:complete"
			}
		}
		if strings.Contains(c, markerFixApplied) {
			if newestMarker == "" {
				newestMarker = "fix:applied"
			}
		}
		// A generic step marker such as [step:3/7] — but not [step:complete],
		// which is handled above and must not also count as "step in progress".
		if strings.Contains(c, markerStepPrefix) && !strings.Contains(c, markerStepComplete) {
			hasStep = true
			if newestMarker == "" {
				newestMarker = "step"
			}
		}
		if strings.Contains(c, markerPhasePlanCreated) {
			hasPlanCreated = true
			if newestMarker == "" {
				newestMarker = "phase:plan-created"
			}
		}
		if strings.Contains(c, markerAnalysisOneStepOK) {
			hasAnalysisOneStepProceed = true
			if newestMarker == "" {
				newestMarker = "analysis:onestep-proceed"
			}
		}
		if strings.Contains(c, markerAnalysisOneStepReject) {
			hasAnalysisOneStepRejected = true
			if newestMarker == "" {
				newestMarker = "analysis:onestep-rejected"
			}
		}
		if strings.Contains(c, markerAnalysisProceed) {
			hasAnalysisProceed = true
			if newestMarker == "" {
				newestMarker = "analysis:proceed"
			}
		}
		if strings.Contains(c, markerAnalysisNeedInfo) {
			hasAnalysisNeedInfo = true
			if newestMarker == "" {
				newestMarker = "analysis:need-info"
			}
		}
	}

	// All implementation is complete — covers both multi-step and onestep
	// finish paths.
	if hasStepComplete {
		return "", "step:complete — ticket finished"
	}
	if hasStep {
		if newestMarker == "fix:applied" || newestMarker == "step" {
			return "", "step in progress or fix pushed, waiting for PR event"
		}
		// Newest marker is stale (e.g. plan-created) — we're actually
		// mid-step and should resume step work.
		return ModeStep, "resuming step work (stale non-step newest marker)"
	}
	if hasPlanCreated {
		if newestMarker == "fix:applied" {
			return "", "fix pushed for plan PR, waiting for re-review"
		}
		return "", "plan created, waiting for plan PR merge"
	}
	if hasAnalysisOneStepProceed {
		if newestMarker == "fix:applied" {
			return "", "fix pushed for onestep PR, waiting for re-review"
		}
		return ModeOnestep, "analysis approved one-step path"
	}
	if hasAnalysisOneStepRejected {
		return "", "analysis rejected one-step — waiting for human"
	}
	if hasAnalysisProceed {
		return ModePlan, "analysis approved proceed"
	}
	if hasAnalysisNeedInfo {
		if hasNewerNonMarkerComment(comments) {
			return ModeAnalysis, "human replied after need-info — re-running analysis"
		}
		return "", "analysis needs info — waiting for human"
	}
	if !hasAnalysisProceed && !hasPlanCreated && !hasStep {
		return ModeAnalysis, "no lifecycle markers — starting analysis"
	}
	return ModeAnalysis, "fallback to analysis"
}

// findFixTarget returns the first open PR that has a non-stale
// REQUEST_CHANGES review.
func findFixTarget(prs []PRSnapshot) *FixTarget {
	for _, snap := range prs {
		if snap.PR.State != prStateOpen {
			continue
		}
		for _, r := range snap.Reviews {
			if r.State == reviewStateRequestChanges && !r.Stale {
				return &FixTarget{
					Owner:  snap.Owner,
					Repo:   snap.Repo,
					Number: snap.PR.Number,
				}
			}
		}
	}
	return nil
}

// findReviewTarget returns the first open PR that has no automated
// review on record for its current head revision, or nil when every
// open PR is already reviewed at its latest commit.
//
// This is the whole "only review again when new commits were added"
// rule, and it is derived entirely from Gitea: the PR's head SHA versus
// the revisions recorded on the reviews already posted by review
// agents. Nothing is remembered between passes.
//
// A PR whose head SHA is unknown is skipped rather than reviewed. An
// empty SHA can never be matched by a later review, so reviewing it
// would re-review the same PR on every single pass — an unbounded token
// burn. Skipping loses a review; not skipping loses the budget.
func findReviewTarget(prs []PRSnapshot) *ReviewTarget {
	for _, snap := range prs {
		if snap.PR.State != prStateOpen {
			continue
		}
		headSHA := snap.PR.Head.Sha
		if headSHA == "" {
			continue
		}
		if hasAgentReviewForSHA(snap.Reviews, headSHA) {
			continue
		}
		if countAgentReviews(snap.Reviews) >= maxAgentReviewsPerPR {
			continue
		}
		return &ReviewTarget{
			Owner:   snap.Owner,
			Repo:    snap.Repo,
			Number:  snap.PR.Number,
			HeadSHA: headSHA,
		}
	}
	return nil
}

// maxAgentReviewsPerPR caps how many automated reviews a single pull
// request may accumulate.
//
// The SHA check already stops the normal case, but it only works if the
// review actually lands on the PR. If posting keeps failing — the
// reviewer lost read access, Gitea rejects the payload, the Job dies
// before posting — no marker is ever recorded, the PR looks unreviewed
// on every pass, and the orchestrator spawns a reviewer every 30
// seconds forever. This cap turns that from an unbounded token burn
// into at most ten wasted runs. Ten also comfortably exceeds the number
// of revisions a real PR goes through.
const maxAgentReviewsPerPR = 10

// countAgentReviews returns how many reviews on a PR were posted by
// review agents.
func countAgentReviews(reviews []gitea.PRReview) int {
	n := 0
	for _, r := range reviews {
		if strings.HasPrefix(r.User.Login, ReviewAgentPrefix) {
			n++
		}
	}
	return n
}

// hasAgentReviewForSHA reports whether any review agent has already
// reviewed the given revision.
//
// Two independent signals are accepted, because neither is reliable
// alone: Gitea's commit_id is not consistently populated on COMMENT
// reviews, and a review body could in principle be edited. Either one
// matching is treated as "already reviewed" — the failure mode of a
// false positive (one skipped review) is far cheaper than that of a
// false negative (re-reviewing the same revision every pass).
func hasAgentReviewForSHA(reviews []gitea.PRReview, headSHA string) bool {
	marker := ReviewSHAMarker(headSHA)
	for _, r := range reviews {
		if !strings.HasPrefix(r.User.Login, ReviewAgentPrefix) {
			continue
		}
		if r.CommitID == headSHA || strings.Contains(r.Body, marker) {
			return true
		}
	}
	return false
}

// modeFromPRState recovers a follow-up mode from merged-PR state. This
// catches the case where a plan or step PR was merged while the
// orchestrator was down (or otherwise missed the merge webhook) — on the
// next reconcile pass we look at Gitea directly and pick up where we
// left off.
//
// Returns "" when no follow-up is warranted: no PRs, a PR still open, or
// all step work already marked complete.
func modeFromPRState(prs []PRSnapshot, comments []taiga.HistoryEntry) (string, string) {
	hasMergedPlanPR := false
	hasMergedStepPR := false
	hasOpenPR := false

	for _, snap := range prs {
		if snap.PR.State == prStateOpen {
			hasOpenPR = true
			continue
		}
		if !snap.PR.Merged {
			continue
		}
		if isPlanPR(snap.PR.Title) {
			hasMergedPlanPR = true
		} else {
			hasMergedStepPR = true
		}
	}

	if hasOpenPR {
		return "", "PR still open, waiting for review/merge"
	}
	if hasMergedStepPR {
		if hasStepCompleteMarker(comments) {
			return "", "step:complete already recorded"
		}
		return ModeStep, "step PR merged, continuing with next step"
	}
	if hasMergedPlanPR {
		return ModeStep, "plan PR merged, starting step work"
	}
	return "", "no merged PRs to recover from"
}

// isPlanPR classifies a PR as a plan PR by title substring. Plan agents
// open PRs titled "Implementation Plan for <ticket>" or similar.
func isPlanPR(title string) bool {
	return strings.Contains(title, planPRTitleMarker) ||
		strings.Contains(title, planPRTitleMarkerLower)
}

// hasStepCompleteMarker returns true if any comment contains
// [step:complete]. Exported only as part of the unexported decision
// logic; callers should use DeriveTicketState, not this helper.
func hasStepCompleteMarker(comments []taiga.HistoryEntry) bool {
	for _, entry := range comments {
		if strings.Contains(entry.Comment, markerStepComplete) {
			return true
		}
	}
	return false
}

// containsAnyLifecycleMarker reports whether a comment string contains
// any recognised lifecycle marker.
func containsAnyLifecycleMarker(c string) bool {
	return strings.Contains(c, markerStepComplete) ||
		strings.Contains(c, markerStepPrefix) ||
		strings.Contains(c, markerFixApplied) ||
		strings.Contains(c, markerPhasePlanCreated) ||
		strings.Contains(c, markerAnalysisOneStepOK) ||
		strings.Contains(c, markerAnalysisOneStepReject) ||
		strings.Contains(c, markerAnalysisProceed) ||
		strings.Contains(c, markerAnalysisNeedInfo)
}

// hasNewerNonMarkerComment reports whether the newest comment in the
// slice (index 0, since comments are sorted newest-first) does not
// contain any lifecycle marker. This detects the case where a human
// replied with additional information after an agent posted
// [analysis:need-info].
func hasNewerNonMarkerComment(comments []taiga.HistoryEntry) bool {
	if len(comments) == 0 {
		return false
	}
	return !containsAnyLifecycleMarker(comments[0].Comment)
}
