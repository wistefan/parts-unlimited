package reconciler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
	"github.com/wistefan/dev-env/orchestrator/pkg/taiga"
)

// fakeTaiga is a scripted TaigaAPI used by the reconciler tests. Callers
// populate ReadyStories, InProgressStories, and CommentsByID; the fake
// serves those values on the corresponding calls and records call counts
// so tests can assert on them.
type fakeTaiga struct {
	readyStatusID      int
	inProgressStatusID int

	readyStories      []taiga.UserStory
	inProgressStories []taiga.UserStory
	commentsByID      map[int][]taiga.HistoryEntry

	listCalls    int
	commentCalls int
	getCalls     int
}

func (f *fakeTaiga) ListUserStories(projectID int, opts *taiga.UserStoryListOptions) ([]taiga.UserStory, error) {
	f.listCalls++
	if opts == nil {
		return nil, fmt.Errorf("unexpected nil opts")
	}
	switch opts.StatusID {
	case f.readyStatusID:
		return f.readyStories, nil
	case f.inProgressStatusID:
		return f.inProgressStories, nil
	default:
		return nil, fmt.Errorf("fakeTaiga: unknown StatusID %d", opts.StatusID)
	}
}

func (f *fakeTaiga) GetUserStory(id int) (*taiga.UserStory, error) {
	f.getCalls++
	for i := range f.readyStories {
		if f.readyStories[i].ID == id {
			s := f.readyStories[i]
			s.Status = f.readyStatusID
			return &s, nil
		}
	}
	for i := range f.inProgressStories {
		if f.inProgressStories[i].ID == id {
			s := f.inProgressStories[i]
			s.Status = f.inProgressStatusID
			return &s, nil
		}
	}
	// Default: return a story with an unknown status so callers can
	// exercise the "not actionable" skip path.
	return &taiga.UserStory{ID: id, Status: -1}, nil
}

func (f *fakeTaiga) GetComments(storyID int) ([]taiga.HistoryEntry, error) {
	f.commentCalls++
	return f.commentsByID[storyID], nil
}

// fakeGitea is a scripted GiteaAPI.
type fakeGitea struct {
	prsByTicket map[int][]gitea.PullRequest
	reviewsByPR map[int][]gitea.PRReview
	listPRCalls int
	reviewCalls int
}

func (f *fakeGitea) ListPullRequestsForTicket(owner, repo string, ticketID int) ([]gitea.PullRequest, error) {
	f.listPRCalls++
	return f.prsByTicket[ticketID], nil
}

func (f *fakeGitea) GetPRReviews(owner, repo string, prNumber int) ([]gitea.PRReview, error) {
	f.reviewCalls++
	return f.reviewsByPR[prNumber], nil
}

// fakeJobs is a scripted JobAPI.
type fakeJobs struct {
	ticketsWithJobs map[int]bool
	activeCount     int
	busy            map[string]bool
	reaped          int
	callCount       int
}

func (f *fakeJobs) HasJobForTicket(ctx context.Context, ticketID int) (bool, error) {
	f.callCount++
	return f.ticketsWithJobs[ticketID], nil
}
func (f *fakeJobs) ActiveJobCount(ctx context.Context) (int, error) {
	return f.activeCount, nil
}
func (f *fakeJobs) ListActiveAgents(ctx context.Context) (map[string]bool, error) {
	if f.busy == nil {
		return map[string]bool{}, nil
	}
	return f.busy, nil
}
func (f *fakeJobs) ReapFinishedJobs(ctx context.Context) (int, error) {
	return f.reaped, nil
}

// fakeSpawner records each Spawn call. Returns errors when FailAll is set.
type fakeSpawner struct {
	calls   []spawnCall
	failAll bool
}

type spawnCall struct {
	TicketID int
	Mode     string
	Fix      *FixTarget
	Busy     map[string]bool
}

func (s *fakeSpawner) Spawn(ctx context.Context, ticketID int, d Decision, busy map[string]bool) error {
	if s.failAll {
		return fmt.Errorf("synthetic spawn failure")
	}
	s.calls = append(s.calls, spawnCall{
		TicketID: ticketID,
		Mode:     d.Mode,
		Fix:      d.Fix,
		Busy:     busy,
	})
	return nil
}

// recordingLogger captures Printf calls.
type recordingLogger struct {
	lines []string
}

func (l *recordingLogger) Printf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func makeStory(id int, subject string) taiga.UserStory {
	return taiga.UserStory{ID: id, Subject: subject}
}

// Shadow-mode tests — reconciler never calls Spawner.

func TestReconcileAll_ShadowMode_LogsWithoutSpawning(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
		humanID           = 42
		ticketWithJob     = 10
		freshTicket       = 11
	)

	taigaFake := &fakeTaiga{
		readyStatusID:      readyID,
		inProgressStatusID: inProgID,
		readyStories:       []taiga.UserStory{makeStory(freshTicket, "fresh ready ticket")},
		inProgressStories:  []taiga.UserStory{makeStory(ticketWithJob, "step-phase ticket")},
		commentsByID: map[int][]taiga.HistoryEntry{
			ticketWithJob: {
				{Comment: "[step:2/5] working\n**Repo:** `claude/example`"},
				{Comment: "[phase:plan-created]\n**Repo:** `claude/example`"},
			},
			freshTicket: {},
		},
	}
	giteaFake := &fakeGitea{}
	jobsFake := &fakeJobs{
		ticketsWithJobs: map[int]bool{ticketWithJob: true},
		activeCount:     1,
		busy:            map[string]bool{"claude-1": true},
	}
	spawner := &fakeSpawner{}
	log := &recordingLogger{}

	r := New(Config{
		Taiga:          taigaFake,
		Gitea:          giteaFake,
		Jobs:           jobsFake,
		Spawner:        spawner,
		Log:            log,
		Mode:           ModeShadow,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		HumanTaigaID:   humanID,
		MaxConcurrency: 3,
	})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	if len(spawner.calls) != 0 {
		t.Fatalf("shadow mode must not call Spawner; got %d calls: %+v", len(spawner.calls), spawner.calls)
	}
	joined := strings.Join(log.lines, "\n")
	if !strings.Contains(joined, "mode: shadow") {
		t.Errorf("expected shadow mode name in log. Got:\n%s", joined)
	}
}

// Authoritative-mode tests — reconciler spawns via Spawner.

func TestReconcileAll_AuthoritativeMode_SpawnsAnalysisForFreshTicket(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
		freshTicket       = 11
	)
	taigaFake := &fakeTaiga{
		readyStatusID:      readyID,
		inProgressStatusID: inProgID,
		readyStories:       []taiga.UserStory{makeStory(freshTicket, "fresh")},
		commentsByID:       map[int][]taiga.HistoryEntry{freshTicket: {}},
	}
	jobsFake := &fakeJobs{activeCount: 0, busy: map[string]bool{}}
	spawner := &fakeSpawner{}
	r := New(Config{
		Taiga:          taigaFake,
		Gitea:          &fakeGitea{},
		Jobs:           jobsFake,
		Spawner:        spawner,
		Mode:           ModeAuthoritative,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		MaxConcurrency: 3,
	})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(spawner.calls))
	}
	if got := spawner.calls[0]; got.TicketID != freshTicket || got.Mode != ModeAnalysis {
		t.Errorf("spawn call = %+v, want ticketID=%d mode=%s", got, freshTicket, ModeAnalysis)
	}
}

func TestReconcileAll_AuthoritativeMode_EnforcesConcurrencyCap(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
	)
	// Three ready tickets, cap is 2, no active jobs. Expect 2 spawns;
	// the third is deferred with a "cap reached" log.
	taigaFake := &fakeTaiga{
		readyStatusID:      readyID,
		inProgressStatusID: inProgID,
		readyStories: []taiga.UserStory{
			makeStory(1, "t1"), makeStory(2, "t2"), makeStory(3, "t3"),
		},
		commentsByID: map[int][]taiga.HistoryEntry{1: {}, 2: {}, 3: {}},
	}
	spawner := &fakeSpawner{}
	log := &recordingLogger{}
	r := New(Config{
		Taiga:          taigaFake,
		Gitea:          &fakeGitea{},
		Jobs:           &fakeJobs{activeCount: 0, busy: map[string]bool{}},
		Spawner:        spawner,
		Log:            log,
		Mode:           ModeAuthoritative,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		MaxConcurrency: 2,
	})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if len(spawner.calls) != 2 {
		t.Errorf("expected 2 spawns under cap, got %d", len(spawner.calls))
	}
	if !strings.Contains(strings.Join(log.lines, "\n"), "maxConcurrency 2 reached") {
		t.Errorf("expected cap-reached log line. Got:\n%s", strings.Join(log.lines, "\n"))
	}
}

func TestReconcileAll_AuthoritativeMode_SkipsTicketsWithExistingJob(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
	)
	taigaFake := &fakeTaiga{
		readyStatusID:      readyID,
		inProgressStatusID: inProgID,
		readyStories: []taiga.UserStory{
			makeStory(1, "has job"), makeStory(2, "fresh"),
		},
		commentsByID: map[int][]taiga.HistoryEntry{1: {}, 2: {}},
	}
	spawner := &fakeSpawner{}
	r := New(Config{
		Taiga: taigaFake,
		Gitea: &fakeGitea{},
		Jobs: &fakeJobs{
			ticketsWithJobs: map[int]bool{1: true},
			activeCount:     1,
			busy:            map[string]bool{"claude-1": true},
		},
		Spawner:        spawner,
		Mode:           ModeAuthoritative,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		MaxConcurrency: 5,
	})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("expected 1 spawn (skipping the job-owning ticket), got %d: %+v",
			len(spawner.calls), spawner.calls)
	}
	if spawner.calls[0].TicketID != 2 {
		t.Errorf("expected spawn for fresh ticket #2, got ticket #%d", spawner.calls[0].TicketID)
	}
	if !spawner.calls[0].Busy["claude-1"] {
		t.Errorf("expected busy agents map to be forwarded to Spawner. Got: %+v", spawner.calls[0].Busy)
	}
}

func TestReconcileAll_AuthoritativeMode_SpawnsFixWithPRTarget(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
		ticketID          = 20
		prNumber          = 77
	)
	taigaFake := &fakeTaiga{
		readyStatusID:      readyID,
		inProgressStatusID: inProgID,
		inProgressStories:  []taiga.UserStory{makeStory(ticketID, "pr needs fix")},
		commentsByID: map[int][]taiga.HistoryEntry{
			ticketID: {
				{Comment: "[step:1/3]\n**Repo:** `claude/repo1`"},
			},
		},
	}
	giteaFake := &fakeGitea{
		prsByTicket: map[int][]gitea.PullRequest{
			ticketID: {{Number: prNumber, State: prStateOpen, Head: gitea.PRRef{Ref: "ticket-20/step-1"}}},
		},
		reviewsByPR: map[int][]gitea.PRReview{
			prNumber: {{State: reviewStateRequestChanges, Stale: false}},
		},
	}
	spawner := &fakeSpawner{}
	r := New(Config{
		Taiga:          taigaFake,
		Gitea:          giteaFake,
		Jobs:           &fakeJobs{},
		Spawner:        spawner,
		Mode:           ModeAuthoritative,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		MaxConcurrency: 3,
	})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if len(spawner.calls) != 1 || spawner.calls[0].Mode != ModeFix {
		t.Fatalf("expected 1 fix spawn, got %+v", spawner.calls)
	}
	if spawner.calls[0].Fix == nil || spawner.calls[0].Fix.Number != prNumber {
		t.Errorf("expected fix target PR #%d, got %+v", prNumber, spawner.calls[0].Fix)
	}
}

func TestReconcileAll_AuthoritativeMode_SkipsTicketsAssignedToHuman(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
		humanID           = 42
	)
	s := makeStory(1, "human-owned")
	h := humanID
	s.AssignedTo = &h
	taigaFake := &fakeTaiga{
		readyStatusID:      readyID,
		inProgressStatusID: inProgID,
		readyStories:       []taiga.UserStory{s},
		commentsByID:       map[int][]taiga.HistoryEntry{1: {}},
	}
	spawner := &fakeSpawner{}
	r := New(Config{
		Taiga:          taigaFake,
		Gitea:          &fakeGitea{},
		Jobs:           &fakeJobs{},
		Spawner:        spawner,
		Mode:           ModeAuthoritative,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		HumanTaigaID:   humanID,
		MaxConcurrency: 3,
	})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if len(spawner.calls) != 0 {
		t.Errorf("human-assigned ticket must not be spawned. Got: %+v", spawner.calls)
	}
}

func TestReconcileAll_AuthoritativeMode_SpawnFailureDoesNotCountAgainstCap(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
	)
	// Two tickets, cap is 2, one spawn will fail — the second should
	// still be attempted.
	taigaFake := &fakeTaiga{
		readyStatusID:      readyID,
		inProgressStatusID: inProgID,
		readyStories:       []taiga.UserStory{makeStory(1, "t1"), makeStory(2, "t2")},
		commentsByID:       map[int][]taiga.HistoryEntry{1: {}, 2: {}},
	}
	failing := &failingSpawner{failOn: 1, recorded: []int{}}
	r := New(Config{
		Taiga:          taigaFake,
		Gitea:          &fakeGitea{},
		Jobs:           &fakeJobs{},
		Spawner:        failing,
		Mode:           ModeAuthoritative,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		MaxConcurrency: 2,
	})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if len(failing.recorded) != 1 || failing.recorded[0] != 2 {
		t.Errorf("expected ticket #2 to spawn after #1 failed. Got: %v", failing.recorded)
	}
}

// failingSpawner returns an error for a single ticket ID; records the
// rest as successful.
type failingSpawner struct {
	failOn   int
	recorded []int
}

func (s *failingSpawner) Spawn(ctx context.Context, ticketID int, d Decision, busy map[string]bool) error {
	if ticketID == s.failOn {
		return fmt.Errorf("synthetic failure for ticket %d", ticketID)
	}
	s.recorded = append(s.recorded, ticketID)
	return nil
}

// ReconcileTicket tests.

func TestReconcileTicket_ActionableTicketIsProcessed(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
		targetID          = 40
	)
	taigaFake := &fakeTaiga{
		readyStatusID:      readyID,
		inProgressStatusID: inProgID,
		readyStories:       []taiga.UserStory{makeStory(targetID, "target")},
		commentsByID:       map[int][]taiga.HistoryEntry{targetID: {}},
	}
	spawner := &fakeSpawner{}
	r := New(Config{
		Taiga:          taigaFake,
		Gitea:          &fakeGitea{},
		Jobs:           &fakeJobs{},
		Spawner:        spawner,
		Mode:           ModeAuthoritative,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		MaxConcurrency: 3,
	})
	if err := r.ReconcileTicket(context.Background(), targetID); err != nil {
		t.Fatalf("ReconcileTicket: %v", err)
	}
	if len(spawner.calls) != 1 || spawner.calls[0].TicketID != targetID {
		t.Errorf("expected one spawn for #%d, got %+v", targetID, spawner.calls)
	}
	if taigaFake.getCalls != 1 {
		t.Errorf("expected 1 GetUserStory call (targeted fetch), got %d", taigaFake.getCalls)
	}
}

func TestReconcileTicket_NonActionableStatusIsSkipped(t *testing.T) {
	log := &recordingLogger{}
	r := New(Config{
		Taiga:          &fakeTaiga{readyStatusID: 100, inProgressStatusID: 200},
		Gitea:          &fakeGitea{},
		Jobs:           &fakeJobs{},
		Spawner:        &fakeSpawner{},
		Log:            log,
		Mode:           ModeAuthoritative,
		ProjectID:      1,
		ReadyStatusID:  100,
		InProgressID:   200,
		MaxConcurrency: 3,
	})
	if err := r.ReconcileTicket(context.Background(), 999); err != nil {
		t.Fatalf("ReconcileTicket: %v", err)
	}
	joined := strings.Join(log.lines, "\n")
	if !strings.Contains(joined, "not in actionable status") {
		t.Errorf("expected skip log. Got:\n%s", joined)
	}
}

func TestReconcileAll_ContinuesPastPerTicketErrors(t *testing.T) {
	const (
		readyID, inProgID = 100, 200
		projectID         = 1
		goodID            = 30
		badID             = 31
	)
	taigaFake := &errCommentsTaiga{
		base: fakeTaiga{
			readyStatusID:      readyID,
			inProgressStatusID: inProgID,
			readyStories: []taiga.UserStory{
				makeStory(goodID, "good ticket"),
				makeStory(badID, "broken comments"),
			},
			commentsByID: map[int][]taiga.HistoryEntry{goodID: {}},
		},
		failOn: map[int]bool{badID: true},
	}
	log := &recordingLogger{}
	spawner := &fakeSpawner{}
	r := New(Config{
		Taiga:          taigaFake,
		Gitea:          &fakeGitea{},
		Jobs:           &fakeJobs{},
		Spawner:        spawner,
		Log:            log,
		Mode:           ModeAuthoritative,
		ProjectID:      projectID,
		ReadyStatusID:  readyID,
		InProgressID:   inProgID,
		MaxConcurrency: 3,
	})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll should not fail on per-ticket errors: %v", err)
	}
	if len(spawner.calls) != 1 || spawner.calls[0].TicketID != goodID {
		t.Errorf("expected good ticket to spawn, got %+v", spawner.calls)
	}
	joined := strings.Join(log.lines, "\n")
	if !strings.Contains(joined, fmt.Sprintf("ticket #%d: fetching comments failed", badID)) {
		t.Errorf("expected error log for broken ticket.\nGot:\n%s", joined)
	}
}

type errCommentsTaiga struct {
	base   fakeTaiga
	failOn map[int]bool
}

func (e *errCommentsTaiga) ListUserStories(projectID int, opts *taiga.UserStoryListOptions) ([]taiga.UserStory, error) {
	return e.base.ListUserStories(projectID, opts)
}

func (e *errCommentsTaiga) GetUserStory(id int) (*taiga.UserStory, error) {
	return e.base.GetUserStory(id)
}

func (e *errCommentsTaiga) GetComments(storyID int) ([]taiga.HistoryEntry, error) {
	if e.failOn[storyID] {
		return nil, fmt.Errorf("synthetic comments error for ticket %d", storyID)
	}
	return e.base.GetComments(storyID)
}
