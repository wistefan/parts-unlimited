package reconciler

import (
	"context"
	"fmt"

	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
	"github.com/wistefan/dev-env/orchestrator/pkg/taiga"
)

// Mode selects whether the reconciler acts or only observes.
type Mode int

const (
	// ModeShadow runs the decision core on every pass and logs the
	// verdict, but never spawns agents or mutates any system. Retained
	// for tests that exercise the read-only decision tree without a
	// real Spawner.
	ModeShadow Mode = iota

	// ModeAuthoritative runs the decision core AND spawns agents via
	// the configured Spawner, enforcing the maxConcurrency cap using
	// K8s-derived busy-agent counts. This is the production mode.
	ModeAuthoritative
)

// TaigaAPI is the Taiga surface the reconciler needs. The production
// *taiga.Client satisfies it directly; tests supply a fake.
type TaigaAPI interface {
	ListUserStories(projectID int, opts *taiga.UserStoryListOptions) ([]taiga.UserStory, error)
	GetUserStory(id int) (*taiga.UserStory, error)
	GetComments(storyID int) ([]taiga.HistoryEntry, error)
}

// GiteaAPI is the Gitea surface the reconciler needs.
type GiteaAPI interface {
	ListPullRequestsForTicket(owner, repo string, ticketID int) ([]gitea.PullRequest, error)
	GetPRReviews(owner, repo string, prNumber int) ([]gitea.PRReview, error)
}

// JobAPI is the Kubernetes-job surface the reconciler needs. It covers
// existence checks for a single ticket, the global active count (for
// enforcing maxConcurrency), the busy-agent set (so the spawner does
// not hand the same agent identity to two concurrent tickets), and
// cleanup of finished Jobs so their tickets can be re-spawned without
// waiting for K8s TTL.
type JobAPI interface {
	HasJobForTicket(ctx context.Context, ticketID int) (bool, error)
	ActiveJobCount(ctx context.Context) (int, error)
	ListActiveAgents(ctx context.Context) (map[string]bool, error)
	ReapFinishedJobs(ctx context.Context) (int, error)
}

// Spawner is the caller-supplied hook that turns a [Decision] into a
// running Kubernetes Job. In shadow mode the reconciler never calls
// Spawn; in authoritative mode it is called once per ticket per pass
// (at most) after cap and existing-job checks.
//
// busyAgents is the K8s-derived set of agent IDs currently attached to
// an active Job; the spawner should pass it through to the identity
// manager so the same agent is not handed out twice.
type Spawner interface {
	Spawn(ctx context.Context, ticketID int, d Decision, busyAgents map[string]bool) error
}

// Logger is the minimal logging surface. The standard library's
// *log.Logger satisfies it.
type Logger interface {
	Printf(format string, args ...any)
}

// Config holds the configuration for a [Reconciler].
type Config struct {
	Taiga         TaigaAPI
	Gitea         GiteaAPI
	Jobs          JobAPI
	Spawner       Spawner // required when Mode == ModeAuthoritative
	Log           Logger
	Mode          Mode
	ProjectID     int
	ReadyStatusID int
	InProgressID  int
	HumanTaigaID  int

	// MaxConcurrency caps the number of active agent Jobs the reconciler
	// will tolerate. When the cap is reached, further Spawn decisions
	// for the current pass are deferred until a later tick. Only honoured
	// in ModeAuthoritative.
	MaxConcurrency int
}

// Reconciler derives per-ticket actions from Taiga and Gitea on every
// pass. It holds no persisted state of its own — the clients it wraps
// are the only sources of truth.
type Reconciler struct {
	cfg Config
}

// New constructs a Reconciler from Config.
func New(cfg Config) *Reconciler {
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	return &Reconciler{cfg: cfg}
}

// ReconcileAll runs one pass over every actionable ticket in the
// project (status=ready or status=in-progress), derives the intended
// action for each, and — in authoritative mode — dispatches to the
// Spawner subject to the maxConcurrency cap.
//
// Errors fetching the ticket list fail the pass. Per-ticket errors are
// logged and the pass continues — one failing ticket must not starve
// the others.
func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	// In authoritative mode, reap finished Jobs first so their tickets
	// are not blocked by the HasJobForTicket guard on this same pass.
	// Shadow mode skips this — the legacy loop is doing cleanup.
	if r.cfg.Mode == ModeAuthoritative {
		if n, err := r.cfg.Jobs.ReapFinishedJobs(ctx); err != nil {
			r.cfg.Log.Printf("[reconciler] reap finished jobs failed: %v", err)
		} else if n > 0 {
			r.cfg.Log.Printf("[reconciler] reaped %d finished jobs", n)
		}
	}

	stories, err := r.listActionableStories()
	if err != nil {
		return fmt.Errorf("listing actionable tickets: %w", err)
	}

	active, busy, err := r.jobSnapshot(ctx)
	if err != nil {
		r.cfg.Log.Printf("[reconciler] job snapshot failed: %v", err)
		// Proceed in best-effort mode: treat zero active jobs and empty
		// busy set. DeriveTicketState still produces useful verdicts; in
		// authoritative mode the per-ticket HasJobForTicket check is the
		// last line of defence against duplicate spawns.
	}

	r.cfg.Log.Printf("[reconciler] pass over %d actionable tickets (active jobs: %d, mode: %s)",
		len(stories), active, r.modeName())

	for i := range stories {
		if r.cfg.Mode == ModeAuthoritative && active >= r.cfg.MaxConcurrency {
			r.cfg.Log.Printf("[reconciler] maxConcurrency %d reached, deferring remaining %d tickets",
				r.cfg.MaxConcurrency, len(stories)-i)
			break
		}
		if spawned := r.reconcileOne(ctx, &stories[i], busy, active); spawned {
			active++
		}
	}
	return nil
}

// ReconcileTicket runs a pass for a single ticket. Called by webhooks
// as a targeted nudge so we do not have to scan the whole project
// after every event.
func (r *Reconciler) ReconcileTicket(ctx context.Context, ticketID int) error {
	story, err := r.cfg.Taiga.GetUserStory(ticketID)
	if err != nil {
		return fmt.Errorf("fetching ticket #%d: %w", ticketID, err)
	}
	if story.Status != r.cfg.ReadyStatusID && story.Status != r.cfg.InProgressID {
		r.cfg.Log.Printf("[reconciler] ticket #%d not in actionable status (status=%d), skipping",
			ticketID, story.Status)
		return nil
	}
	_, busy, err := r.jobSnapshot(ctx)
	if err != nil {
		r.cfg.Log.Printf("[reconciler] job snapshot failed: %v", err)
	}
	// ReconcileTicket does not pre-check the global concurrency cap:
	// a webhook that wants to spawn an urgent fix should not be starved
	// by background passes. HasJobForTicket still prevents duplicates.
	r.reconcileOne(ctx, story, busy, 0)
	return nil
}

// listActionableStories concatenates Taiga's ready and in-progress
// statuses. Ready stories are listed first so a fresh ticket gets a
// pass before in-progress ones whose state may be waiting on a PR.
func (r *Reconciler) listActionableStories() ([]taiga.UserStory, error) {
	ready, err := r.cfg.Taiga.ListUserStories(r.cfg.ProjectID, &taiga.UserStoryListOptions{
		StatusID: r.cfg.ReadyStatusID,
	})
	if err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	inProgress, err := r.cfg.Taiga.ListUserStories(r.cfg.ProjectID, &taiga.UserStoryListOptions{
		StatusID: r.cfg.InProgressID,
	})
	if err != nil {
		return nil, fmt.Errorf("in-progress: %w", err)
	}
	return append(ready, inProgress...), nil
}

// jobSnapshot collects the two K8s-derived quantities the pass needs:
// the current active-job count (for cap enforcement) and the set of
// busy agent IDs (for identity handout).
func (r *Reconciler) jobSnapshot(ctx context.Context) (int, map[string]bool, error) {
	active, err := r.cfg.Jobs.ActiveJobCount(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("active count: %w", err)
	}
	busy, err := r.cfg.Jobs.ListActiveAgents(ctx)
	if err != nil {
		return active, nil, fmt.Errorf("busy agents: %w", err)
	}
	return active, busy, nil
}

// reconcileOne is the per-ticket worker. Returns true iff a spawn was
// issued — ReconcileAll uses this to increment its in-pass active
// counter and stop before exceeding the cap.
func (r *Reconciler) reconcileOne(ctx context.Context, story *taiga.UserStory, busy map[string]bool, active int) bool {
	comments, err := r.cfg.Taiga.GetComments(story.ID)
	if err != nil {
		r.cfg.Log.Printf("[reconciler] ticket #%d: fetching comments failed: %v", story.ID, err)
		return false
	}

	owner, name := FindRepoForTicket(comments)
	prs, err := r.fetchPRSnapshots(owner, name, story.ID)
	if err != nil {
		r.cfg.Log.Printf("[reconciler] ticket #%d: fetching PRs for %s/%s failed: %v",
			story.ID, owner, name, err)
		// Continue with an empty PR list — DeriveTicketState will produce
		// the best decision it can from comment markers alone.
		prs = nil
	}

	decision := DeriveTicketState(story, comments, prs, r.cfg.HumanTaigaID)

	hasJob, jobErr := r.cfg.Jobs.HasJobForTicket(ctx, story.ID)
	if jobErr != nil {
		r.cfg.Log.Printf("[reconciler] ticket #%d: job lookup failed: %v", story.ID, jobErr)
	}

	r.logDecision(story, decision, hasJob)

	if r.cfg.Mode != ModeAuthoritative {
		return false
	}
	if decision.Action != ActionSpawn {
		return false
	}
	if hasJob {
		// A Job already exists for this ticket (running, or queued for
		// cleanup). Leave it alone — it will either complete and trigger
		// the next decision, or be collected by a future pass.
		return false
	}
	if active >= r.cfg.MaxConcurrency {
		r.cfg.Log.Printf("[reconciler] ticket #%d: deferring spawn (cap %d reached)",
			story.ID, r.cfg.MaxConcurrency)
		return false
	}
	if err := r.cfg.Spawner.Spawn(ctx, story.ID, decision, busy); err != nil {
		r.cfg.Log.Printf("[reconciler] ticket #%d: spawn failed: %v", story.ID, err)
		return false
	}
	return true
}

// fetchPRSnapshots turns the ticket's repo (if known) into the PRSnapshot
// list DeriveTicketState expects. When the repo cannot be resolved
// (first-agent case, no Repo: marker yet) we return an empty list — at
// that point there are no PRs to consider anyway.
func (r *Reconciler) fetchPRSnapshots(owner, name string, ticketID int) ([]PRSnapshot, error) {
	if owner == "" || name == "" {
		return nil, nil
	}
	prs, err := r.cfg.Gitea.ListPullRequestsForTicket(owner, name, ticketID)
	if err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	snapshots := make([]PRSnapshot, 0, len(prs))
	for _, pr := range prs {
		snap := PRSnapshot{Owner: owner, Repo: name, PR: pr}
		// Reviews are only needed for open PRs (fix detection). Closed
		// and merged PRs never trigger ModeFix, so skip the API call.
		if pr.State == prStateOpen {
			reviews, err := r.cfg.Gitea.GetPRReviews(owner, name, pr.Number)
			if err != nil {
				r.cfg.Log.Printf("[reconciler] ticket #%d: fetching reviews for PR #%d failed: %v",
					ticketID, pr.Number, err)
			} else {
				snap.Reviews = reviews
			}
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

// logDecision emits a single structured log line per ticket. Operators
// grep `[reconciler]` for the pass output and the action word
// (`SPAWN` / `NONE`) to see exactly why the reconciler made its choice.
func (r *Reconciler) logDecision(story *taiga.UserStory, d Decision, hasJob bool) {
	jobState := "no-job"
	if hasJob {
		jobState = "job-exists"
	}
	switch d.Action {
	case ActionSpawn:
		if d.Mode == ModeFix && d.Fix != nil {
			r.cfg.Log.Printf("[reconciler] ticket #%d (%s): SPAWN %s on %s/%s#%d [%s] — %s",
				story.ID, story.Subject, d.Mode, d.Fix.Owner, d.Fix.Repo, d.Fix.Number, jobState, d.Reason)
		} else {
			r.cfg.Log.Printf("[reconciler] ticket #%d (%s): SPAWN %s [%s] — %s",
				story.ID, story.Subject, d.Mode, jobState, d.Reason)
		}
	case ActionNone:
		r.cfg.Log.Printf("[reconciler] ticket #%d (%s): NONE [%s] — %s",
			story.ID, story.Subject, jobState, d.Reason)
	}
}

func (r *Reconciler) modeName() string {
	if r.cfg.Mode == ModeAuthoritative {
		return "authoritative"
	}
	return "shadow"
}

// nopLogger is the zero-value logger used when Config.Log is nil.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}
