// Package main is the entrypoint for the orchestrator service.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/wistefan/dev-env/orchestrator/pkg/assignment"
	"github.com/wistefan/dev-env/orchestrator/pkg/config"
	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
	"github.com/wistefan/dev-env/orchestrator/pkg/identity"
	"github.com/wistefan/dev-env/orchestrator/pkg/lifecycle"
	"github.com/wistefan/dev-env/orchestrator/pkg/metrics"
	"github.com/wistefan/dev-env/orchestrator/pkg/notifications"
	"github.com/wistefan/dev-env/orchestrator/pkg/reconciler"
	"github.com/wistefan/dev-env/orchestrator/pkg/review"
	"github.com/wistefan/dev-env/orchestrator/pkg/taiga"
	"github.com/wistefan/dev-env/orchestrator/pkg/webhooks"
)

const (
	// WebhookListenPort is the port the orchestrator listens on for webhooks.
	WebhookListenPort = 8080

	// WebhookName is the name used when registering the Taiga webhook.
	WebhookName = "orchestrator"

	// ReconcileInterval is how often the orchestrator polls for missed events.
	ReconcileInterval = 30 * time.Second

	// StatusReady is the Taiga status name for tickets available to agents.
	StatusReady = "ready"

	// StatusInProgress is the Taiga status name for tickets being worked on.
	StatusInProgress = "in progress"

	// StatusReadyForTest is the Taiga status name for completed tickets.
	StatusReadyForTest = "ready for test"
)

// orchestrator holds all initialized subsystems.
type orchestrator struct {
	cfg                 *config.Config
	taigaClient         *taiga.Client
	giteaClient         *gitea.Client
	lifecycleMgr        *lifecycle.Manager
	identityMgr         *identity.Manager
	notifySvc           *notifications.Service
	reviewSvc           *review.Service
	webhookHandler      *webhooks.Handler
	giteaWebhookHandler *webhooks.GiteaHandler

	projectID      int
	readyStatusID  int
	inProgressID   int
	readyForTestID int
	humanTaigaID   int

	// registeredRepos tracks repos that have a Gitea webhook registered.
	// Prevents duplicate webhook registrations within a single run.
	registeredRepos map[string]bool

	// reconcilerSvc is the stateless Taiga+Gitea-derived reconciler — the
	// single spawn path. Webhooks nudge it via ReconcileTicket; the
	// periodic loop runs ReconcileAll on every tick.
	reconcilerSvc *reconciler.Reconciler
}

func main() {
	configPath := flag.String("config", "/etc/orchestrator/config.yaml", "Path to configuration file")
	flag.Parse()

	cfg, err := config.LoadFromFile(*configPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("Config file not found, using defaults")
			cfg = config.DefaultConfig()
		} else {
			log.Fatalf("Failed to load config: %v", err)
		}
	}

	log.Printf("Orchestrator starting...")
	log.Printf("  Gitea:  %s", cfg.Gitea.URL)
	log.Printf("  Taiga:  %s", cfg.Taiga.URL)
	log.Printf("  Max concurrency: %d", cfg.Agents.MaxConcurrency)
	log.Printf("  Namespace: %s", cfg.Kubernetes.Namespace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := initialize(ctx, cfg)
	if err != nil {
		log.Fatalf("Initialization failed: %v", err)
	}

	// Register webhook event handlers
	orch.registerWebhookHandlers()
	orch.registerGiteaWebhookHandlers()

	// Generate a webhook secret if none was configured
	if cfg.Taiga.WebhookSecret == "" {
		secret, err := generateSecret()
		if err != nil {
			log.Fatalf("Failed to generate webhook secret: %v", err)
		}
		cfg.Taiga.WebhookSecret = secret
		orch.webhookHandler = webhooks.NewHandler(secret)
		log.Printf("  Generated webhook secret (none was configured)")
	}

	// Register the webhook with Taiga
	if err := orch.registerTaigaWebhook(ctx); err != nil {
		log.Printf("WARNING: Could not register Taiga webhook: %v", err)
		log.Printf("  Webhooks must be configured manually or the orchestrator will rely on polling only.")
	}

	// Queue depth is the count of tickets in "ready" status. The
	// reconciler will pick them up on its next pass; they are the
	// effective queue, sourced from Taiga directly.
	metrics.RegisterQueueDepth(orch.readyTicketCount)

	// Start HTTP server
	mux := http.NewServeMux()
	mux.Handle("/webhooks/taiga", orch.webhookHandler)
	mux.Handle("/webhooks/gitea", orch.giteaWebhookHandler)
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", WebhookListenPort),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// The stateless reconciler is the single spawn path.
	go orch.runReconcilerLoop(ctx, orch.reconcilerSvc)
	log.Printf("Reconciler running (interval: %s)", ReconcileInterval)

	// Start server in goroutine
	go func() {
		log.Printf("Listening on :%d", WebhookListenPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	log.Printf("Received signal %v, shutting down...", sig)

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("Orchestrator stopped.")
}

// initialize creates and connects all subsystems.
func initialize(ctx context.Context, cfg *config.Config) (*orchestrator, error) {
	// Kubernetes client (in-cluster config)
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}

	// Taiga client
	taigaClient := taiga.NewClient(cfg.Taiga.URL)
	if err := taigaClient.Authenticate(cfg.Taiga.AdminUsername, cfg.Taiga.AdminPassword); err != nil {
		return nil, fmt.Errorf("taiga authentication: %w", err)
	}
	log.Println("  Taiga: authenticated")

	// Resolve project
	project, err := taigaClient.GetProjectBySlug(cfg.Taiga.ProjectSlug)
	if err != nil {
		return nil, fmt.Errorf("resolving taiga project %q: %w", cfg.Taiga.ProjectSlug, err)
	}
	log.Printf("  Taiga: project %q (ID: %d)", project.Name, project.ID)

	// Resolve status IDs
	statuses, err := taigaClient.ListStatuses(project.ID)
	if err != nil {
		return nil, fmt.Errorf("listing taiga statuses: %w", err)
	}
	statusMap := make(map[string]int)
	for _, s := range statuses {
		statusMap[strings.ToLower(s.Name)] = s.ID
	}

	readyStatusID, ok := statusMap[StatusReady]
	if !ok {
		return nil, fmt.Errorf("taiga status %q not found", StatusReady)
	}
	inProgressID, ok := statusMap[StatusInProgress]
	if !ok {
		return nil, fmt.Errorf("taiga status %q not found", StatusInProgress)
	}
	readyForTestID := statusMap[StatusReadyForTest] // 0 if not found (optional)
	log.Printf("  Taiga: statuses resolved (ready=%d, in_progress=%d, ready_for_test=%d)", readyStatusID, inProgressID, readyForTestID)

	// Resolve role for agent memberships. Agents need to comment on tickets,
	// update status, and manage assignments — pick the role with the most
	// permissions (typically the admin/product-owner role).
	roles, err := taigaClient.ListRoles(project.ID)
	if err != nil {
		return nil, fmt.Errorf("listing taiga roles: %w", err)
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("no roles found for taiga project")
	}
	agentRoleID := roles[0].ID
	maxPerms := len(roles[0].Permissions)
	for _, r := range roles {
		if len(r.Permissions) > maxPerms {
			agentRoleID = r.ID
			maxPerms = len(r.Permissions)
		}
	}

	// Resolve human user ID for ticket reassignment
	var humanTaigaID int
	if cfg.Taiga.HumanUsername != "" {
		members, err := taigaClient.ListProjectMembers(project.ID)
		if err != nil {
			log.Printf("WARNING: Could not list project members: %v", err)
		} else {
			for _, member := range members {
				if member.Username == cfg.Taiga.HumanUsername {
					humanTaigaID = member.ID
					break
				}
			}
		}
		if humanTaigaID > 0 {
			log.Printf("  Taiga: human user %q (ID: %d)", cfg.Taiga.HumanUsername, humanTaigaID)
		} else {
			log.Printf("WARNING: Human user %q not found in project members", cfg.Taiga.HumanUsername)
		}
	}

	// Gitea client
	giteaClient := gitea.NewClient(cfg.Gitea.URL, cfg.Gitea.AdminUsername, cfg.Gitea.AdminPassword)
	log.Println("  Gitea: client initialized")

	// Subsystems
	lcConfig := &lifecycle.Config{
		Namespace:          cfg.Kubernetes.Namespace,
		ContainerImage:     cfg.Agents.ContainerImage,
		ServiceAccount:     cfg.Kubernetes.AgentServiceAccount,
		TaskTimeoutSeconds: int64(cfg.Agents.TaskTimeoutSeconds),
		RetryLimit:         int32(cfg.Agents.RetryLimit),
		TTLAfterFinished:   lifecycle.DefaultTTLAfterFinished,
	}
	lifecycleMgr := lifecycle.NewManager(clientset, lcConfig)

	identityMgr := identity.NewManager(giteaClient, taigaClient, clientset, k8sConfig, project.ID, agentRoleID)
	if err := identityMgr.HydrateFromGitea(); err != nil {
		log.Printf("WARNING: identity hydrate from Gitea failed: %v", err)
	}

	notifySvc := notifications.NewService(notifications.Config{
		WebhookURL:    cfg.Notifications.WebhookURL,
		DesktopNotify: cfg.Notifications.DesktopNotify,
	})

	reviewSvc := review.NewService(giteaClient, review.DefaultConfig())

	webhookHandler := webhooks.NewHandler(cfg.Taiga.WebhookSecret)
	giteaWebhookHandler := webhooks.NewGiteaHandler("") // secret set later if configured

	orch := &orchestrator{
		cfg:                 cfg,
		taigaClient:         taigaClient,
		giteaClient:         giteaClient,
		lifecycleMgr:        lifecycleMgr,
		identityMgr:         identityMgr,
		notifySvc:           notifySvc,
		reviewSvc:           reviewSvc,
		webhookHandler:      webhookHandler,
		giteaWebhookHandler: giteaWebhookHandler,
		projectID:           project.ID,
		readyStatusID:       readyStatusID,
		inProgressID:        inProgressID,
		readyForTestID:      readyForTestID,
		humanTaigaID:        humanTaigaID,
		registeredRepos:     make(map[string]bool),
	}

	// Construct the stateless reconciler — the single spawn path. The
	// Spawner is wired here so it can close over the orchestrator's
	// existing spawn helpers.
	orch.reconcilerSvc = reconciler.New(reconciler.Config{
		Taiga:          taigaClient,
		Gitea:          giteaClient,
		Jobs:           lifecycleMgr,
		Spawner:        &orchSpawner{o: orch},
		Log:            log.Default(),
		Mode:           reconciler.ModeAuthoritative,
		ProjectID:      project.ID,
		ReadyStatusID:  readyStatusID,
		InProgressID:   inProgressID,
		HumanTaigaID:   humanTaigaID,
		MaxConcurrency: cfg.Agents.MaxConcurrency,
	})

	log.Println("Initialization complete.")
	return orch, nil
}

// registerWebhookHandlers sets up event handlers for Taiga webhook events.
func (o *orchestrator) registerWebhookHandlers() {
	// User story changed — status transitions, comments, description edits, tag changes
	o.webhookHandler.OnFunc("userstory", "change", func(event *webhooks.WebhookEvent) error {
		data, err := webhooks.ParseUserStoryData(event)
		if err != nil {
			return err
		}

		statusName := data.Status.Name
		log.Printf("Ticket #%d: change by %s, status=%q", data.ID, event.By.Username, statusName)

		// Ticket moved to "ready" — nudge the reconciler.
		if strings.EqualFold(statusName, StatusReady) {
			log.Printf("Ticket #%d: status is ready, nudging reconciler", data.ID)
			o.nudgeReconciler(data.ID)
			return nil
		}

		// Ticket became unassigned — the human finished providing input.
		// Re-run analysis regardless of ticket status (ready or in-progress).
		if o.isTicketUnassigned(event) {
			log.Printf("Ticket #%d: unassigned, nudging reconciler", data.ID)
			o.nudgeReconciler(data.ID)
			return nil
		}

		// Check for delegation tags
		return o.handleTagChange(event)
	})

	// New user story created — check if it starts in "ready" status
	o.webhookHandler.OnFunc("userstory", "create", func(event *webhooks.WebhookEvent) error {
		data, err := webhooks.ParseUserStoryData(event)
		if err != nil {
			return err
		}
		if strings.EqualFold(data.Status.Name, StatusReady) {
			log.Printf("New ticket #%d created in ready status, nudging reconciler", data.ID)
			o.nudgeReconciler(data.ID)
		}
		return nil
	})
}

// registerGiteaWebhookHandlers sets up event handlers for Gitea webhook events.
func (o *orchestrator) registerGiteaWebhookHandlers() {
	// PR opened — trigger auto-review
	o.giteaWebhookHandler.On(webhooks.GiteaEventPROpened, func(_ string, event *webhooks.GiteaPREvent) error {
		log.Printf("Gitea: PR #%d opened on %s by %s",
			event.PullRequest.Number, event.Repository.FullName, event.Sender.Login)

		// Trigger auto-review in a goroutine so the webhook returns fast.
		parts := strings.SplitN(event.Repository.FullName, "/", 2)
		prOwner, prRepo := parts[0], parts[1]
		prNum := event.PullRequest.Number

		go o.runAutoReview(prOwner, prRepo, prNum)

		return nil
	})

	// PR merged — re-spawn agent for next step
	o.giteaWebhookHandler.On(webhooks.GiteaEventPRMerged, func(_ string, event *webhooks.GiteaPREvent) error {
		ticketID := o.resolveTicketFromPR(event)
		if ticketID == 0 {
			log.Printf("Gitea: PR #%d merged on %s — no ticket mapping, ignoring",
				event.PullRequest.Number, event.Repository.FullName)
			return nil
		}

		log.Printf("Gitea: PR #%d merged on %s for ticket #%d",
			event.PullRequest.Number, event.Repository.FullName, ticketID)

		// Check the agent's last Taiga comment for step markers
		if o.isStepComplete(ticketID) {
			log.Printf("Gitea: ticket #%d all steps complete — transitioning to ready-for-test", ticketID)
			o.transitionToReadyForTest(ticketID)
			return nil
		}

		log.Printf("Gitea: ticket #%d PR merged — nudging reconciler", ticketID)
		o.nudgeReconciler(ticketID)
		return nil
	})

	// PR closed without merge — escalate
	o.giteaWebhookHandler.On(webhooks.GiteaEventPRClosed, func(_ string, event *webhooks.GiteaPREvent) error {
		ticketID := o.resolveTicketFromPR(event)
		if ticketID == 0 {
			return nil
		}

		prNum := event.PullRequest.Number
		// Per-PR marker lets us dedupe redelivered webhooks without
		// touching unrelated rejection comments on the same ticket (a
		// human could close multiple PRs against one ticket).
		rejectionMarker := fmt.Sprintf("[pr-rejected:%d]", prNum)

		// Dedupe: Gitea redelivers pull_request:closed on retries, and a
		// human may also close/reopen/close. Without this guard every
		// delivery posts another "awaiting human guidance" comment and
		// re-reassigns the ticket.
		if comments, err := o.taigaClient.GetComments(ticketID); err == nil {
			for _, c := range comments {
				if strings.Contains(c.Comment, rejectionMarker) {
					log.Printf("Gitea: PR #%d close for ticket #%d already escalated — skipping duplicate webhook",
						prNum, ticketID)
					return nil
				}
			}
		} else {
			log.Printf("WARNING: could not fetch comments for ticket %d (dedupe check): %v", ticketID, err)
		}

		log.Printf("Gitea: PR #%d rejected (closed without merge) on %s for ticket #%d — escalating",
			prNum, event.Repository.FullName, ticketID)

		// Post escalation comment on Taiga ticket
		story, err := o.taigaClient.GetUserStory(ticketID)
		if err != nil {
			log.Printf("WARNING: could not fetch ticket %d for escalation: %v", ticketID, err)
			return nil
		}
		comment := fmt.Sprintf("%s\n\n**PR #%d was rejected** (closed without merge) by %s.\n\nTicket paused — awaiting human guidance.",
			rejectionMarker, prNum, event.Sender.Login)
		_, err = o.taigaClient.UpdateUserStory(ticketID, &taiga.UserStoryUpdate{
			Comment:    comment,
			AssignedTo: &o.humanTaigaID,
			Version:    story.Version,
		})
		if err != nil {
			log.Printf("WARNING: could not post escalation comment on ticket %d: %v", ticketID, err)
		}

		return nil
	})

	// PR review: request changes — spawn PR-fix agent
	o.giteaWebhookHandler.On(webhooks.GiteaEventReviewRequestChanges, func(_ string, event *webhooks.GiteaPREvent) error {
		// Only react to human reviews, not agent reviews
		if strings.Contains(event.Sender.Login, "-agent-") {
			return nil
		}

		ticketID := o.resolveTicketFromPR(event)
		if ticketID == 0 {
			return nil
		}

		log.Printf("Gitea: human %s requested changes on PR #%d (ticket #%d) — nudging reconciler",
			event.Sender.Login, event.PullRequest.Number, ticketID)
		o.nudgeReconciler(ticketID)
		return nil
	})
}

// isStepComplete checks the latest Taiga comments for [step:complete] marker.
func (o *orchestrator) isStepComplete(ticketID int) bool {
	comments, err := o.taigaClient.GetComments(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch comments for ticket %d: %v", ticketID, err)
		return false
	}

	// Search from newest to oldest (API returns newest-first)
	for i := 0; i < len(comments); i++ {
		c := comments[i].Comment
		if strings.Contains(c, "[step:complete]") {
			return true
		}
		// If we find a [step:N/M] marker first, there are more steps
		if strings.Contains(c, "[step:") {
			return false
		}
	}
	return false
}

// transitionToReadyForTest moves a ticket to "ready for test" status,
// assigns it to the human user, and posts release notes.
func (o *orchestrator) transitionToReadyForTest(ticketID int) {
	story, err := o.taigaClient.GetUserStory(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch ticket %d: %v", ticketID, err)
		return
	}

	update := &taiga.UserStoryUpdate{
		Version: story.Version,
	}

	if o.readyForTestID > 0 {
		update.Status = &o.readyForTestID
	}
	if o.humanTaigaID > 0 {
		update.AssignedTo = &o.humanTaigaID
	}

	_, err = o.taigaClient.UpdateUserStory(ticketID, update)
	if err != nil {
		log.Printf("WARNING: could not transition ticket %d to ready-for-test: %v", ticketID, err)
	}

	o.notifySvc.Notify(
		notifications.EventReadyForTest,
		ticketID, "",
		fmt.Sprintf("Ticket #%d ready for test", ticketID),
		"All implementation steps completed. Please review and test.",
		"",
	)

	log.Printf("Ticket #%d transitioned to ready-for-test", ticketID)
}

// ensureGiteaWebhook registers a Gitea webhook on a repo if not already registered.
// Called when the orchestrator first encounters a repo (e.g., agent creates a PR).
func (o *orchestrator) ensureGiteaWebhook(owner, repo string) {
	key := owner + "/" + repo
	if o.registeredRepos[key] {
		return
	}

	webhookURL := fmt.Sprintf("http://orchestrator.agents.svc.cluster.local:%d/webhooks/gitea", WebhookListenPort)
	err := o.giteaClient.CreateRepoWebhook(owner, repo, &gitea.CreateHookRequest{
		Type: "gitea",
		Config: map[string]string{
			"url":          webhookURL,
			"content_type": "json",
		},
		Events: []string{"pull_request", "pull_request_review", "pull_request_rejected"},
		Active: true,
	})
	if err != nil {
		// May already exist — not fatal
		log.Printf("Gitea webhook registration on %s/%s: %v (may already exist)", owner, repo, err)
	} else {
		log.Printf("Gitea webhook registered on %s/%s", owner, repo)
	}
	o.registeredRepos[key] = true
}

// runAutoReview assigns a PR to the human user for review.
// TODO: invoke Claude Code to post an automated review comment before assigning.
// Currently the orchestrator's distroless image doesn't include the Claude CLI,
// so the automated review is skipped — only the PR assignment is performed.
func (o *orchestrator) runAutoReview(owner, repo string, prNumber int) {
	log.Printf("Auto-review: assigning PR #%d on %s/%s to human for review", prNumber, owner, repo)

	// Assign PR to the human user
	if o.cfg.Taiga.HumanUsername != "" {
		_, err := o.giteaClient.EditPullRequest(owner, repo, prNumber, &gitea.EditPRRequest{
			Assignees: []string{o.cfg.Taiga.HumanUsername},
		})
		if err != nil {
			log.Printf("Auto-review: WARNING: could not assign PR #%d to human: %v", prNumber, err)
		}
	}

	log.Printf("Auto-review: PR #%d assigned to %s", prNumber, o.cfg.Taiga.HumanUsername)
}

// resolveTicketFromPR looks up the Taiga ticket ID for a Gitea PR
// event by parsing it out of the PR title or body. Every
// orchestrator-managed PR uses a `ticket-{id}/...` branch convention
// the parser also recognises, so this is the single source of truth
// — no in-memory mapping needed.
func (o *orchestrator) resolveTicketFromPR(event *webhooks.GiteaPREvent) int {
	return webhooks.ParseTicketIDFromPRBody(event.PullRequest.Title, event.PullRequest.Body)
}

// isTicketUnassigned checks whether a webhook event represents a ticket becoming
// unassigned (assigned_to changed from a non-null value to null).  This is the
// signal that the human finished providing input and the ticket is ready for
// (re-)analysis.
func (o *orchestrator) isTicketUnassigned(event *webhooks.WebhookEvent) bool {
	if event.Change == nil || event.Change.Diff == nil {
		return false
	}

	var diff map[string]json.RawMessage
	if err := json.Unmarshal(event.Change.Diff, &diff); err != nil {
		return false
	}

	raw, ok := diff["assigned_to"]
	if !ok {
		return false
	}

	// Parse the "from"/"to" structure: {"from": <value>, "to": null}
	var change struct {
		From json.RawMessage `json:"from"`
		To   json.RawMessage `json:"to"`
	}
	if err := json.Unmarshal(raw, &change); err != nil {
		return false
	}

	// "to" is null (unassigned) and "from" was non-null (was previously assigned)
	fromIsNull := change.From == nil || string(change.From) == "null"
	toIsNull := change.To == nil || string(change.To) == "null"

	return toIsNull && !fromIsNull
}

// transitionToInProgress moves a ticket to the "in progress" Taiga status.
// Extracted so both the proceed and onestep-proceed analysis outcomes share
// the same status transition without duplicating fetch-and-patch plumbing.
func (o *orchestrator) transitionToInProgress(ticketID int) {
	story, err := o.taigaClient.GetUserStory(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch ticket %d: %v", ticketID, err)
		return
	}
	_, err = o.taigaClient.UpdateUserStory(ticketID, &taiga.UserStoryUpdate{
		Status:  &o.inProgressID,
		Version: story.Version,
	})
	if err != nil {
		log.Printf("WARNING: could not update ticket %d to in-progress: %v", ticketID, err)
	}
}

// isAssignedToHuman checks whether the ticket is currently assigned to the human user.
func (o *orchestrator) isAssignedToHuman(data *webhooks.UserStoryData) bool {
	if o.humanTaigaID == 0 {
		return false
	}
	if data.AssignedTo != nil && data.AssignedTo.ID == o.humanTaigaID {
		return true
	}
	for _, uid := range data.AssignedUsers {
		if uid == o.humanTaigaID {
			return true
		}
	}
	return false
}

// ticketAssignedToHuman fetches the ticket from Taiga and reports
// whether it is currently assigned to the configured human user (as
// primary assignee or watcher). Returns false on fetch errors — we
// fail open so a transient Taiga hiccup doesn't stall work. Agents
// set the human as assignee when they block on input (see
// `request_human_input`), so this is the orchestrator's signal to
// stay out of the way: no spawns, no comments, no assignments until
// the human releases the ticket.
func (o *orchestrator) ticketAssignedToHuman(ticketID int) bool {
	if o.humanTaigaID == 0 {
		return false
	}
	story, err := o.taigaClient.GetUserStory(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch ticket %d to check assignee: %v", ticketID, err)
		return false
	}
	if story.AssignedTo != nil && *story.AssignedTo == o.humanTaigaID {
		return true
	}
	for _, uid := range story.AssignedUsers {
		if uid == o.humanTaigaID {
			return true
		}
	}
	return false
}

// repoMarkerRE parses the repo line every agent embeds in its Taiga
// comment. Tolerates the common variants agents have emitted:
//
//	**Repo:** `owner/name`
//	**Repo:** owner/name
//	Repo: owner/name
//
// This is the orchestrator's only source of truth for which fork a
// ticket lives on — it keeps no local state.
var repoMarkerRE = regexp.MustCompile("(?:\\*\\*)?Repo:(?:\\*\\*)?[\\s`]*([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)")

// findRepoForTicket returns the canonical Gitea owner/name for a ticket
// by scanning its Taiga comment history for a `**Repo:** owner/name`
// marker. Returns ("", "") when no marker exists yet (first-agent case):
// the agent then resolves the repo itself from the ticket description.
//
// The walk is *oldest-first*: the first fork that hosted work for this
// ticket wins, and later comments cannot move the canonical pointer.
// Newest-first was unsafe because a wayward agent (one that ran with a
// different identity / fork than the original) would post a comment
// embedding its own `Repo:` marker, silently rebinding the orchestrator
// to a fork with no merged plan/step PRs. The merged work history lives
// where the plan PR landed, so anchoring on the *first* claim keeps the
// orchestrator looking at the fork that actually owns the ticket.
func (o *orchestrator) findRepoForTicket(ticketID int) (owner, name string) {
	comments, err := o.taigaClient.GetComments(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch comments for ticket %d to resolve repo: %v", ticketID, err)
		return "", ""
	}
	// GetComments returns newest-first; iterate in reverse so the
	// oldest agent comment with a Repo: marker wins.
	for i := len(comments) - 1; i >= 0; i-- {
		if m := repoMarkerRE.FindStringSubmatch(comments[i].Comment); len(m) == 3 {
			return m[1], m[2]
		}
	}
	return "", ""
}

// pinRepoToTicket sets spec.RepoOwner/RepoName from a prior PR on the
// ticket (if any) and grants the spawning agent write access when they
// don't already own the repo. First-agent case (no prior PR) is a
// no-op: spec remains empty and the agent falls back to bootstrap's
// description-based repo resolution.
func (o *orchestrator) pinRepoToTicket(ticketID int, agentID string, spec *lifecycle.AgentJobSpec) {
	owner, name := o.findRepoForTicket(ticketID)
	if owner == "" {
		return
	}
	spec.RepoOwner = owner
	spec.RepoName = name
	if owner == agentID {
		return
	}
	if err := o.giteaClient.AddCollaborator(owner, name, agentID, "write"); err != nil {
		log.Printf("WARNING: ticket #%d: could not grant agent %s write access to %s/%s: %v",
			ticketID, agentID, owner, name, err)
	}
}

// findLastAgentForTicket returns the Taiga/Gitea username of the most
// recent agent that commented on this ticket, derived from Taiga
// comment history (the orchestrator keeps no local state). Continuing
// with the same agent on each iteration keeps branch ownership, the
// pushed IMPLEMENTATION_PLAN.md, and any CLAUDE.md context coherent;
// switching agents mid-ticket loses all of that and the new agent
// often re-does early steps. Returns "" when no agent has commented
// yet (first-agent case).
func (o *orchestrator) findLastAgentForTicket(ticketID int) string {
	comments, err := o.taigaClient.GetComments(ticketID)
	if err != nil {
		return ""
	}
	for _, c := range comments {
		if strings.Contains(c.User.Username, "-agent-") {
			return c.User.Username
		}
	}
	return ""
}

// errPreviousAgentBusy signals that a ticket's prior agent is currently
// busy on another ticket. Callers should defer the spawn — the
// reconciler / queue will retry on the next pass — rather than
// switching to a different agent. Switching mid-ticket splits the work
// history across two forks (different Gitea owner → different branches
// and PRs visible to the orchestrator), and the canonical-fork pointer
// in the latest comment ends up pointing at a fork with no merged
// plan/step PRs, leaving the ticket permanently unrecoverable.
var errPreviousAgentBusy = errors.New("previous agent for ticket is busy")

// assignTicketInTaiga sets the assigned_to field on a Taiga ticket so the
// assignment is visible in the board UI.  It does not change the ticket status.
func (o *orchestrator) assignTicketInTaiga(ticketID, taigaUserID int) {
	story, err := o.taigaClient.GetUserStory(ticketID)
	if err != nil {
		log.Printf("WARNING: Could not fetch ticket %d for assignment: %v", ticketID, err)
		return
	}
	_, err = o.taigaClient.UpdateUserStory(ticketID, &taiga.UserStoryUpdate{
		AssignedTo: &taigaUserID,
		Version:    story.Version,
	})
	if err != nil {
		log.Printf("WARNING: Could not assign ticket %d to agent: %v", ticketID, err)
	}
}

// handleTagChange checks for delegation tags on a user story change event.
func (o *orchestrator) handleTagChange(event *webhooks.WebhookEvent) error {
	data, err := webhooks.ParseUserStoryData(event)
	if err != nil {
		return err
	}

	delegateTags := assignment.ExtractDelegationTags(data.Tags)
	for _, tag := range delegateTags {
		spec := assignment.DelegateToSpecialization(tag)
		if spec == "" {
			continue
		}
		log.Printf("Ticket #%d: delegation requested to %s", data.ID, spec)
		o.handleDelegation(data.ID, spec)
	}
	return nil
}

// handleDelegation creates a specialized agent for delegated work.
func (o *orchestrator) handleDelegation(ticketID int, specialization string) {
	if o.ticketAssignedToHuman(ticketID) {
		log.Printf("Ticket #%d: assigned to human, skipping delegation to %s", ticketID, specialization)
		return
	}
	busy, err := o.lifecycleMgr.ListActiveAgents(context.Background())
	if err != nil {
		log.Printf("ERROR: Could not list active agents for ticket %d delegation: %v", ticketID, err)
		return
	}
	agent, err := o.identityMgr.GetOrCreateAgent(specialization, busy)
	if err != nil {
		log.Printf("ERROR: Could not create %s agent for ticket %d: %v", specialization, ticketID, err)
		return
	}

	spec := &lifecycle.AgentJobSpec{
		AgentID:        agent.ID,
		Specialization: specialization,
		TicketID:       ticketID,
		GiteaUsername:  agent.ID,
		GiteaPassword:  agent.Password,
		TaigaUsername:  agent.ID,
		TaigaPassword:  agent.Password,
		HumanUsername:  o.cfg.Taiga.HumanUsername,
		HumanTaigaID:   o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}

	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		log.Printf("ERROR: Could not create job for delegated work: %v", err)
		return
	}

	log.Printf("Ticket #%d: delegated to %s (job: %s)", ticketID, agent.ID, jobName)
}

// runReconcilerLoop runs the stateless reconciler on a periodic
// interval. The reconciler is the single spawn path.
func (o *orchestrator) runReconcilerLoop(ctx context.Context, r *reconciler.Reconciler) {
	ticker := time.NewTicker(ReconcileInterval)
	defer ticker.Stop()

	// Immediate first pass so operators see reconciler output without
	// having to wait 30s after startup.
	if err := r.ReconcileAll(ctx); err != nil {
		log.Printf("Reconciler error (initial pass): %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.ReconcileAll(ctx); err != nil {
				log.Printf("Reconciler error: %v", err)
			}
		}
	}
}

// orchSpawner adapts the orchestrator's internal spawn helpers to the
// reconciler.Spawner interface. The reconciler decides WHICH mode to
// spawn; the adapter handles agent identity selection, Taiga
// assignment, Gitea collaborator grants for fix mode, and Job creation.
type orchSpawner struct {
	o *orchestrator
}

// Spawn routes a reconciler decision to the orchestrator's spawn path.
// Implements reconciler.Spawner.
func (s *orchSpawner) Spawn(ctx context.Context, ticketID int, d reconciler.Decision, busy map[string]bool) error {
	return s.o.spawnForReconciler(ctx, ticketID, d, busy)
}

// spawnForReconciler is the authoritative spawn path invoked by the
// stateless reconciler. Mirrors spawnAgentForTicket/spawnFixAgent but
// takes the K8s-derived busy-agent set as a parameter rather than
// reading it from the legacy in-memory engine state.
func (o *orchestrator) spawnForReconciler(ctx context.Context, ticketID int, d reconciler.Decision, busy map[string]bool) error {
	if o.ticketAssignedToHuman(ticketID) {
		log.Printf("Ticket #%d: assigned to human, skipping %s spawn", ticketID, d.Mode)
		return nil
	}

	// When the ticket is still in "ready" but we are spawning a
	// non-analysis mode, the analysis phase has already produced a
	// verdict (e.g. [analysis:proceed]) — move the ticket to
	// "in-progress" so Taiga UI reflects active work. Mirrors the
	// transition the legacy handleAnalysisCompletion does.
	if d.Mode != reconciler.ModeAnalysis {
		if story, err := o.taigaClient.GetUserStory(ticketID); err == nil && story.Status == o.readyStatusID {
			o.transitionToInProgress(ticketID)
		}
	}

	agent, err := o.pickAgentForTicketWithBusy(ticketID, busy)
	if errors.Is(err, errPreviousAgentBusy) {
		// Reconciler will retry on the next pass once the prior agent
		// frees up. Not an error condition — just a deferral.
		return nil
	}
	if err != nil {
		return fmt.Errorf("picking agent: %w", err)
	}

	o.assignTicketInTaiga(ticketID, agent.TaigaUserID)

	spec := &lifecycle.AgentJobSpec{
		AgentID:        agent.ID,
		Specialization: agent.Specialization,
		TicketID:       ticketID,
		Mode:           d.Mode,
		GiteaUsername:  agent.ID,
		GiteaPassword:  agent.Password,
		TaigaUsername:  agent.ID,
		TaigaPassword:  agent.Password,
		HumanUsername:  o.cfg.Taiga.HumanUsername,
		HumanTaigaID:   o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}

	if d.Mode == reconciler.ModeFix && d.Fix != nil {
		spec.PRNumber = d.Fix.Number
		spec.PRRepo = d.Fix.Owner + "/" + d.Fix.Repo
		spec.RepoOwner = d.Fix.Owner
		spec.RepoName = d.Fix.Repo
		// Grant write access so the fix branch can be pushed when the
		// fix agent is not the repo owner.
		if d.Fix.Owner != agent.ID {
			if err := o.giteaClient.AddCollaborator(d.Fix.Owner, d.Fix.Repo, agent.ID, "write"); err != nil {
				log.Printf("WARNING: ticket #%d: could not grant fix agent %s write access to %s/%s: %v",
					ticketID, agent.ID, d.Fix.Owner, d.Fix.Repo, err)
			}
		}
	} else {
		o.pinRepoToTicket(ticketID, agent.ID, spec)
	}

	jobName, err := o.lifecycleMgr.CreateJob(ctx, spec)
	if err != nil {
		return fmt.Errorf("creating job: %w", err)
	}
	log.Printf("Ticket #%d: %s agent %s spawned (job: %s)", ticketID, d.Mode, agent.ID, jobName)
	return nil
}

// pickAgentForTicketWithBusy is the explicit-busy-set variant of
// pickAgentForTicket. The reconciler passes a K8s-derived busy-agents
// map so no in-memory engine state is read on the authoritative path.
//
// Sticky-identity semantics (see pickAgentForTicket godoc):
//
//   - No prior agent recorded in Taiga → free choice via GetOrCreateAgent.
//   - Prior agent recorded and known → reuse it, deferring with
//     errPreviousAgentBusy when busy.
//   - Prior agent recorded but unknown → adopt it from Gitea + Taiga.
//     Adoption failure is a hard error, not a silent fall-through to a
//     different agent.
func (o *orchestrator) pickAgentForTicketWithBusy(ticketID int, busy map[string]bool) (*identity.AgentIdentity, error) {
	prev := o.findLastAgentForTicket(ticketID)
	if prev == "" {
		return o.identityMgr.GetOrCreateAgent("general", busy)
	}

	a := o.identityMgr.GetAgent(prev)
	if a == nil {
		adopted, err := o.identityMgr.AdoptExisting(prev)
		if err != nil {
			return nil, fmt.Errorf("ticket #%d previously owned by %s, identity not recoverable: %w", ticketID, prev, err)
		}
		a = adopted
	}

	if busy[prev] {
		log.Printf("Ticket #%d: previous agent %s is busy; deferring spawn", ticketID, prev)
		return nil, errPreviousAgentBusy
	}

	log.Printf("Ticket #%d: continuing with previous agent %s", ticketID, prev)
	return a, nil
}

// readyTicketCount returns the number of Taiga user stories currently
// in "ready" status. Used as the orchestrator_queue_depth metric on
// each Prometheus scrape. Errors are logged and the gauge reports 0,
// preserving the rule that no orchestrator-side state crosses passes.
func (o *orchestrator) readyTicketCount() int {
	stories, err := o.taigaClient.ListUserStories(o.projectID, &taiga.UserStoryListOptions{
		StatusID: o.readyStatusID,
	})
	if err != nil {
		log.Printf("WARNING: queue-depth metric: listing ready tickets: %v", err)
		return 0
	}
	return len(stories)
}

// nudgeReconciler runs ReconcileTicket in a goroutine so the webhook
// can return quickly. The reconciler itself is safe for concurrent
// calls — see reconciler.Reconciler.
func (o *orchestrator) nudgeReconciler(ticketID int) {
	if o.reconcilerSvc == nil {
		return
	}
	go func() {
		if err := o.reconcilerSvc.ReconcileTicket(context.Background(), ticketID); err != nil {
			log.Printf("ERROR: reconciler nudge for ticket %d failed: %v", ticketID, err)
		}
	}()
}

// registerTaigaWebhook registers the orchestrator's webhook endpoint with Taiga.
func (o *orchestrator) registerTaigaWebhook(ctx context.Context) error {
	// Determine the webhook callback URL (in-cluster)
	callbackURL := fmt.Sprintf("http://orchestrator.%s.svc.cluster.local:%d/webhooks/taiga",
		o.cfg.Kubernetes.Namespace, WebhookListenPort)

	// Delete any existing webhook with the same name so we can recreate
	// it with the current secret (the secret changes on every restart
	// when not explicitly configured).
	existing, err := o.taigaClient.ListWebhooks(o.projectID)
	if err != nil {
		return fmt.Errorf("listing webhooks: %w", err)
	}
	for _, wh := range existing {
		if wh.Name == WebhookName {
			log.Printf("Deleting existing Taiga webhook (ID: %d) to update secret", wh.ID)
			if err := o.taigaClient.DeleteWebhook(wh.ID); err != nil {
				return fmt.Errorf("deleting old webhook: %w", err)
			}
		}
	}

	_, err = o.taigaClient.CreateWebhook(o.projectID, WebhookName, callbackURL, o.cfg.Taiga.WebhookSecret)
	if err != nil {
		return fmt.Errorf("creating webhook: %w", err)
	}

	log.Printf("Taiga webhook registered: %s", callbackURL)
	return nil
}

// generateSecret creates a random hex-encoded secret for webhook HMAC verification.
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
