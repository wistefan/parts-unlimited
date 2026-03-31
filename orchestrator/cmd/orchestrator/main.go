// Package main is the entrypoint for the orchestrator service.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/wistefan/dev-env/orchestrator/pkg/assignment"
	"github.com/wistefan/dev-env/orchestrator/pkg/config"
	"github.com/wistefan/dev-env/orchestrator/pkg/gitea"
	"github.com/wistefan/dev-env/orchestrator/pkg/identity"
	"github.com/wistefan/dev-env/orchestrator/pkg/lifecycle"
	"github.com/wistefan/dev-env/orchestrator/pkg/notifications"
	"github.com/wistefan/dev-env/orchestrator/pkg/review"
	"github.com/wistefan/dev-env/orchestrator/pkg/state"
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
	assignEngine        *assignment.Engine
	lifecycleMgr        *lifecycle.Manager
	identityMgr         *identity.Manager
	stateMgr            *state.Manager
	notifySvc           *notifications.Service
	reviewSvc           *review.Service
	webhookHandler      *webhooks.Handler
	giteaWebhookHandler *webhooks.GiteaHandler

	projectID      int
	readyStatusID  int
	inProgressID   int
	readyForTestID int
	humanTaigaID   int

	// prMappings maps "{owner}/{repo}#{pr_number}" → ticket ID.
	// Populated from agent completion comments; used to route PR events.
	prMappings map[string]int

	// registeredRepos tracks repos that have a Gitea webhook registered.
	// Prevents duplicate webhook registrations within a single run.
	registeredRepos map[string]bool
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

	// Reconcile state on startup (catch missed events)
	if err := orch.reconcile(ctx); err != nil {
		log.Printf("WARNING: Initial reconciliation failed: %v", err)
	}

	// Start HTTP server
	mux := http.NewServeMux()
	mux.Handle("/webhooks/taiga", orch.webhookHandler)
	mux.Handle("/webhooks/gitea", orch.giteaWebhookHandler)
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

	// Start reconciliation loop
	go orch.reconcileLoop(ctx)

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

	// Save final state
	orch.saveState(context.Background())
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
	assignEngine := assignment.NewEngine(cfg.Agents.MaxConcurrency, cfg.Agents.EscalationThreshold)

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

	stateMgr := state.NewManager(clientset, cfg.Kubernetes.Namespace)

	notifySvc := notifications.NewService(notifications.Config{
		WebhookURL:    cfg.Notifications.WebhookURL,
		DesktopNotify: cfg.Notifications.DesktopNotify,
	})

	reviewSvc := review.NewService(giteaClient, review.DefaultConfig())

	webhookHandler := webhooks.NewHandler(cfg.Taiga.WebhookSecret)
	giteaWebhookHandler := webhooks.NewGiteaHandler("") // secret set later if configured

	// Restore state from ConfigMap
	savedState, err := stateMgr.Load(ctx)
	if err != nil {
		log.Printf("WARNING: Could not load saved state: %v", err)
	}
	prMappings := make(map[string]int)
	if savedState != nil {
		for _, agent := range savedState.Agents {
			identityMgr.RegisterExisting(&agent)
		}
		if savedState.PRMappings != nil {
			prMappings = savedState.PRMappings
		}
		log.Printf("  State: restored %d agents, %d PR mappings", len(savedState.Agents), len(prMappings))
	}

	orch := &orchestrator{
		cfg:                 cfg,
		taigaClient:         taigaClient,
		giteaClient:         giteaClient,
		assignEngine:        assignEngine,
		lifecycleMgr:        lifecycleMgr,
		identityMgr:         identityMgr,
		stateMgr:            stateMgr,
		notifySvc:           notifySvc,
		reviewSvc:           reviewSvc,
		webhookHandler:      webhookHandler,
		giteaWebhookHandler: giteaWebhookHandler,
		projectID:           project.ID,
		readyStatusID:       readyStatusID,
		inProgressID:        inProgressID,
		readyForTestID:      readyForTestID,
		humanTaigaID:        humanTaigaID,
		prMappings:          prMappings,
		registeredRepos:     make(map[string]bool),
	}

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

		// Ticket moved to "ready" — enqueue for assignment
		if strings.EqualFold(statusName, StatusReady) {
			if o.assignEngine.GetAssignment(data.ID) == nil {
				log.Printf("Ticket #%d: status is ready, enqueuing", data.ID)
				o.assignEngine.Enqueue(data.ID)
				o.processQueue()
			}
			return nil
		}

		// Ticket became unassigned — the human finished providing input.
		// Re-run analysis regardless of ticket status (ready or in-progress).
		if o.isTicketUnassigned(event) {
			if o.assignEngine.GetAssignment(data.ID) == nil {
				log.Printf("Ticket #%d: unassigned, enqueuing for analysis", data.ID)
				o.assignEngine.Enqueue(data.ID)
				o.processQueue()
			} else {
				log.Printf("Ticket #%d: unassigned but already tracked, re-spawning analysis", data.ID)
				o.respawnAgent(data.ID, "analysis")
			}
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
			log.Printf("New ticket #%d created in ready status, enqueuing", data.ID)
			o.assignEngine.Enqueue(data.ID)
			o.processQueue()
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

		// Record PR-to-ticket mapping
		ticketID := o.resolveTicketFromPR(event)
		if ticketID > 0 {
			key := fmt.Sprintf("%s#%d", event.Repository.FullName, event.PullRequest.Number)
			o.prMappings[key] = ticketID
			o.saveState(context.Background())
			log.Printf("Gitea: PR #%d mapped to ticket #%d", event.PullRequest.Number, ticketID)
		}

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
		} else {
			log.Printf("Gitea: ticket #%d has more steps — re-spawning agent", ticketID)
			o.respawnAgent(ticketID, "step")
		}

		return nil
	})

	// PR closed without merge — escalate
	o.giteaWebhookHandler.On(webhooks.GiteaEventPRClosed, func(_ string, event *webhooks.GiteaPREvent) error {
		ticketID := o.resolveTicketFromPR(event)
		if ticketID == 0 {
			return nil
		}

		log.Printf("Gitea: PR #%d rejected (closed without merge) on %s for ticket #%d — escalating",
			event.PullRequest.Number, event.Repository.FullName, ticketID)

		// Post escalation comment on Taiga ticket
		story, err := o.taigaClient.GetUserStory(ticketID)
		if err != nil {
			log.Printf("WARNING: could not fetch ticket %d for escalation: %v", ticketID, err)
			return nil
		}
		comment := fmt.Sprintf("**PR #%d was rejected** (closed without merge) by %s.\n\nTicket paused — awaiting human guidance.",
			event.PullRequest.Number, event.Sender.Login)
		_, err = o.taigaClient.UpdateUserStory(ticketID, &taiga.UserStoryUpdate{
			Comment:    comment,
			AssignedTo: &o.humanTaigaID,
			Version:    story.Version,
		})
		if err != nil {
			log.Printf("WARNING: could not post escalation comment on ticket %d: %v", ticketID, err)
		}

		// Clear internal assignment
		o.assignEngine.CompleteTicket(ticketID)
		o.saveState(context.Background())

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

		log.Printf("Gitea: human %s requested changes on PR #%d (ticket #%d) — spawning fix agent",
			event.Sender.Login, event.PullRequest.Number, ticketID)

		existing := o.assignEngine.GetAssignment(ticketID)
		if existing == nil {
			log.Printf("WARNING: no assignment for ticket %d, cannot spawn fix agent", ticketID)
			return nil
		}

		agent := o.identityMgr.GetAgent(existing.PrimaryAgent)
		if agent == nil {
			log.Printf("WARNING: agent %s not found for ticket %d", existing.PrimaryAgent, ticketID)
			return nil
		}

		// Parse owner/repo from the full name
		parts := strings.SplitN(event.Repository.FullName, "/", 2)
		repoOwner, repoName := parts[0], parts[1]

		spec := &lifecycle.AgentJobSpec{
			AgentID:        agent.ID,
			Specialization: agent.Specialization,
			TicketID:       ticketID,
			Mode:           "fix",
			PRNumber:       event.PullRequest.Number,
			PRRepo:         event.Repository.FullName,
			RepoOwner:      repoOwner,
			RepoName:       repoName,
			GiteaUsername:  agent.ID,
			GiteaPassword:  agent.Password,
			TaigaUsername:  agent.ID,
			TaigaPassword:  agent.Password,
			HumanUsername:  o.cfg.Taiga.HumanUsername,
			HumanTaigaID:  o.humanTaigaID,
			TaigaProjectID: o.projectID,
		}

		jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
		if err != nil {
			log.Printf("ERROR: could not spawn fix agent for ticket %d: %v", ticketID, err)
			return nil
		}
		log.Printf("Gitea: fix agent %s spawned (job: %s) for PR #%d on ticket #%d",
			agent.ID, jobName, event.PullRequest.Number, ticketID)
		o.saveState(context.Background())

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

	// Search from newest to oldest
	for i := len(comments) - 1; i >= 0; i-- {
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

	// Clear the internal assignment
	o.assignEngine.CompleteTicket(ticketID)
	o.saveState(context.Background())

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
		Events: []string{"pull_request", "pull_request_review"},
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

// runAutoReview performs an automated Claude review on a PR.
// It assigns the PR to the reviewing agent during the review, then reassigns
// to the human user after the review is posted.
func (o *orchestrator) runAutoReview(owner, repo string, prNumber int) {
	log.Printf("Auto-review: starting for PR #%d on %s/%s", prNumber, owner, repo)

	// Assign PR to the orchestrator/claude account during review
	_, err := o.giteaClient.EditPullRequest(owner, repo, prNumber, &gitea.EditPRRequest{
		Assignees: []string{o.cfg.Gitea.AdminUsername},
	})
	if err != nil {
		log.Printf("Auto-review: WARNING: could not assign PR #%d to reviewer: %v", prNumber, err)
	}

	// Run the Claude review
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, err = o.reviewSvc.ReviewPR(ctx, owner, repo, prNumber)
	if err != nil {
		log.Printf("Auto-review: ERROR on PR #%d: %v", prNumber, err)
	}

	// Reassign PR to the human user
	if o.cfg.Taiga.HumanUsername != "" {
		_, err = o.giteaClient.EditPullRequest(owner, repo, prNumber, &gitea.EditPRRequest{
			Assignees: []string{o.cfg.Taiga.HumanUsername},
		})
		if err != nil {
			log.Printf("Auto-review: WARNING: could not reassign PR #%d to human: %v", prNumber, err)
		}
	}

	log.Printf("Auto-review: completed for PR #%d on %s/%s", prNumber, owner, repo)
}

// resolveTicketFromPR looks up the Taiga ticket ID for a Gitea PR event.
// First checks the in-memory PR mapping, then falls back to parsing the PR body/title.
func (o *orchestrator) resolveTicketFromPR(event *webhooks.GiteaPREvent) int {
	key := fmt.Sprintf("%s#%d", event.Repository.FullName, event.PullRequest.Number)
	if ticketID, ok := o.prMappings[key]; ok {
		return ticketID
	}
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

// handleJobCompletion processes the result of a completed agent Job based on its mode.
func (o *orchestrator) handleJobCompletion(ctx context.Context, ticketID int, assignment *assignment.TicketAssignment) {
	switch assignment.Mode {
	case "analysis":
		o.handleAnalysisCompletion(ctx, ticketID, assignment)
	default:
		// For plan/step/fix modes, the Job posted comments and created PRs.
		// The Gitea webhook handlers (PR opened/merged/review) drive the
		// next steps.  Here we just mark the Job as done without clearing
		// the assignment — the agent is still "owning" this ticket.
		log.Printf("Ticket #%d: %s Job completed, awaiting PR events", ticketID, assignment.Mode)
	}
}

// handleAnalysisCompletion processes the result of an analysis Job by reading
// the agent's Taiga comment for [analysis:proceed] or [analysis:need-info].
func (o *orchestrator) handleAnalysisCompletion(ctx context.Context, ticketID int, assgn *assignment.TicketAssignment) {
	// Fetch the latest comments on the ticket to find the analysis result
	comments, err := o.taigaClient.GetComments(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch comments for ticket %d: %v", ticketID, err)
		o.assignEngine.CompleteTicket(ticketID)
		return
	}

	// Search comments from newest to oldest for the analysis marker
	analysisResult := ""
	for i := len(comments) - 1; i >= 0; i-- {
		c := comments[i]
		if strings.Contains(c.Comment, "[analysis:proceed]") {
			analysisResult = "proceed"
			break
		}
		if strings.Contains(c.Comment, "[analysis:need-info]") {
			analysisResult = "need-info"
			break
		}
	}

	switch analysisResult {
	case "proceed":
		log.Printf("Ticket #%d: analysis result = proceed, moving to in-progress and spawning plan agent", ticketID)

		// Transition ticket to "in-progress"
		story, err := o.taigaClient.GetUserStory(ticketID)
		if err != nil {
			log.Printf("WARNING: could not fetch ticket %d: %v", ticketID, err)
		} else {
			_, err = o.taigaClient.UpdateUserStory(ticketID, &taiga.UserStoryUpdate{
				Status:  &o.inProgressID,
				Version: story.Version,
			})
			if err != nil {
				log.Printf("WARNING: could not update ticket %d to in-progress: %v", ticketID, err)
			}
		}

		// Spawn the plan agent
		o.assignEngine.SetMode(ticketID, "plan")
		o.respawnAgent(ticketID, "plan")

	case "need-info":
		log.Printf("Ticket #%d: analysis result = need-info, ticket assigned to human", ticketID)
		// The analysis agent already assigned the ticket to the human and
		// posted the info request comment.  Clear the internal assignment
		// so the orchestrator picks it up again when unassigned.
		o.assignEngine.CompleteTicket(ticketID)

	default:
		log.Printf("WARNING: Ticket #%d: no analysis marker found in comments, treating as need-info", ticketID)
		o.assignEngine.CompleteTicket(ticketID)
	}

	o.saveState(ctx)
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

// respawnAgent re-spawns the assigned agent for a ticket with the given mode.
// Used when the orchestrator needs to advance the lifecycle (e.g. after analysis
// confirms proceed, or after a PR merge triggers the next step).
func (o *orchestrator) respawnAgent(ticketID int, mode string) {
	existing := o.assignEngine.GetAssignment(ticketID)
	if existing == nil {
		log.Printf("Ticket #%d: no existing assignment, enqueuing as new work", ticketID)
		o.assignEngine.Enqueue(ticketID)
		o.processQueue()
		return
	}

	agent := o.identityMgr.GetAgent(existing.PrimaryAgent)
	if agent == nil {
		log.Printf("Ticket #%d: agent %s not found, creating new", ticketID, existing.PrimaryAgent)
		var err error
		agent, err = o.identityMgr.GetOrCreateAgent("general", o.assignEngine.GetBusyAgents())
		if err != nil {
			log.Printf("ERROR: Could not get agent for ticket %d: %v", ticketID, err)
			return
		}
	}

	// Delete any existing Job for this agent+ticket (e.g. completed analysis Job)
	// before creating the new one — they share the same name.
	oldJobName := lifecycle.JobName(agent.ID, ticketID)
	if err := o.lifecycleMgr.DeleteJob(context.Background(), oldJobName); err != nil {
		log.Printf("Ticket #%d: could not delete old job %s (may not exist): %v", ticketID, oldJobName, err)
	}

	spec := &lifecycle.AgentJobSpec{
		AgentID:        agent.ID,
		Specialization: agent.Specialization,
		TicketID:       ticketID,
		Mode:           mode,
		GiteaUsername:  agent.ID,
		GiteaPassword:  agent.Password,
		TaigaUsername:  agent.ID,
		TaigaPassword:  agent.Password,
		HumanUsername:  o.cfg.Taiga.HumanUsername,
		HumanTaigaID:  o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}

	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		log.Printf("ERROR: Could not re-spawn agent for ticket %d: %v", ticketID, err)
		return
	}

	log.Printf("Ticket #%d: agent %s re-spawned in %s mode (job: %s)", ticketID, agent.ID, mode, jobName)
	o.saveState(context.Background())
}

// handleTagChange checks for delegation tags on a user story change event.
func (o *orchestrator) handleTagChange(event *webhooks.WebhookEvent) error {
	data, err := webhooks.ParseUserStoryData(event)
	if err != nil {
		return err
	}

	delegateTags := assignment.ExtractDelegationTags(data.Tags)
	for _, tag := range delegateTags {
		spec := o.assignEngine.DelegateToSpecialization(tag)
		if spec == "" {
			continue
		}
		log.Printf("Ticket #%d: delegation requested to %s", data.ID, spec)
		o.handleDelegation(data.ID, spec)
	}
	return nil
}

// processQueue attempts to assign queued tickets to available agents.
func (o *orchestrator) processQueue() {
	for {
		entry := o.assignEngine.Dequeue()
		if entry == nil {
			return
		}

		if err := o.assignTicket(entry.TicketID); err != nil {
			log.Printf("ERROR: Failed to assign ticket %d: %v", entry.TicketID, err)
			// Re-enqueue on failure
			o.assignEngine.Enqueue(entry.TicketID)
			return
		}
	}
}

// assignTicket creates an agent and spawns an analysis Job for the given ticket.
// The ticket stays in "ready" status — it only moves to "in-progress" after the
// analysis agent confirms it can proceed (via a Taiga comment).
func (o *orchestrator) assignTicket(ticketID int) error {
	// Get or create an agent
	agent, err := o.identityMgr.GetOrCreateAgent("general", o.assignEngine.GetBusyAgents())
	if err != nil {
		return fmt.Errorf("getting agent: %w", err)
	}

	// Record the assignment internally — ticket status stays "ready" in Taiga
	o.assignEngine.AssignAgent(ticketID, agent.ID)
	o.assignEngine.SetMode(ticketID, "analysis")

	// Assign the ticket to the agent user in Taiga (makes the assignment visible)
	// but do NOT change the status — it stays "ready" until analysis confirms.
	story, err := o.taigaClient.GetUserStory(ticketID)
	if err != nil {
		log.Printf("WARNING: Could not fetch ticket %d for assignment: %v", ticketID, err)
	} else {
		_, err = o.taigaClient.UpdateUserStory(ticketID, &taiga.UserStoryUpdate{
			AssignedTo: &agent.TaigaUserID,
			Version:    story.Version,
		})
		if err != nil {
			log.Printf("WARNING: Could not assign ticket %d to agent: %v", ticketID, err)
		}
	}

	// Spawn the analysis agent Job
	spec := &lifecycle.AgentJobSpec{
		AgentID:        agent.ID,
		Specialization: agent.Specialization,
		TicketID:       ticketID,
		Mode:           "analysis",
		GiteaUsername:  agent.ID,
		GiteaPassword:  agent.Password,
		TaigaUsername:  agent.ID,
		TaigaPassword:  agent.Password,
		HumanUsername:  o.cfg.Taiga.HumanUsername,
		HumanTaigaID:  o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}

	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		return fmt.Errorf("creating job: %w", err)
	}

	log.Printf("Ticket #%d: analysis assigned to %s (job: %s)", ticketID, agent.ID, jobName)

	// Save state
	o.saveState(context.Background())

	return nil
}

// handleDelegation creates a specialized agent for delegated work.
func (o *orchestrator) handleDelegation(ticketID int, specialization string) {
	agent, err := o.identityMgr.GetOrCreateAgent(specialization, o.assignEngine.GetBusyAgents())
	if err != nil {
		log.Printf("ERROR: Could not create %s agent for ticket %d: %v", specialization, ticketID, err)
		return
	}

	o.assignEngine.RecordDelegation(ticketID, agent.ID)

	spec := &lifecycle.AgentJobSpec{
		AgentID:        agent.ID,
		Specialization: specialization,
		TicketID:       ticketID,
		GiteaUsername:   agent.ID,
		GiteaPassword:   agent.Password,
		TaigaUsername:   agent.ID,
		TaigaPassword:   agent.Password,
		HumanUsername:  o.cfg.Taiga.HumanUsername,
		HumanTaigaID:  o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}

	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		log.Printf("ERROR: Could not create job for delegated work: %v", err)
		return
	}

	log.Printf("Ticket #%d: delegated to %s (job: %s)", ticketID, agent.ID, jobName)
	o.saveState(context.Background())
}

// reconcile checks for tickets in "ready" state that may have been missed.
func (o *orchestrator) reconcile(ctx context.Context) error {
	// Check for "ready" tickets that need assignment
	readyStories, err := o.taigaClient.ListUserStories(o.projectID, &taiga.UserStoryListOptions{
		StatusID: o.readyStatusID,
	})
	if err != nil {
		return fmt.Errorf("listing ready tickets: %w", err)
	}
	for _, story := range readyStories {
		if o.assignEngine.GetAssignment(story.ID) == nil {
			log.Printf("Reconcile: found unassigned ready ticket #%d: %s", story.ID, story.Subject)
			o.assignEngine.Enqueue(story.ID)
		}
	}

	// Check for "in progress" tickets that have no running Job and are
	// not assigned to the human — these need an agent re-spawned.
	inProgressStories, err := o.taigaClient.ListUserStories(o.projectID, &taiga.UserStoryListOptions{
		StatusID: o.inProgressID,
	})
	if err != nil {
		log.Printf("WARNING: Could not list in-progress tickets: %v", err)
	} else {
		for _, story := range inProgressStories {
			// Skip if already assigned in the engine or assigned to the human
			if o.assignEngine.GetAssignment(story.ID) != nil {
				continue
			}
			assignedToHuman := false
			if story.AssignedTo != nil && *story.AssignedTo == o.humanTaigaID {
				assignedToHuman = true
			}
			if assignedToHuman {
				continue
			}
			log.Printf("Reconcile: in-progress ticket #%d has no agent, re-spawning", story.ID)
			o.respawnAgent(story.ID, "step")
		}
	}

	// Check for completed Jobs: process results and clean up finished Jobs
	jobStatuses, err := o.lifecycleMgr.ListActiveJobs(ctx)
	if err != nil {
		log.Printf("WARNING: Could not list active jobs: %v", err)
	} else {
		for _, js := range jobStatuses {
			if js.Succeeded || js.Failed {
				ticketID, _ := strconv.Atoi(js.TicketID)
				assignment := o.assignEngine.GetAssignment(ticketID)
				if assignment != nil {
					if js.Succeeded {
						log.Printf("Reconcile: job %s completed for ticket #%d (mode=%s)", js.Name, ticketID, assignment.Mode)
						o.handleJobCompletion(ctx, ticketID, assignment)
						// handleJobCompletion may have deleted the old Job and
						// created a replacement with the same name (e.g.
						// analysis→plan).  Do NOT delete here or we kill the
						// new Job.  respawnAgent already handles cleanup.
						continue
					}
					log.Printf("Reconcile: job %s failed for ticket #%d", js.Name, ticketID)
					o.assignEngine.CompleteTicket(ticketID)
				}
				// Delete finished/failed Jobs that were not re-spawned
				if err := o.lifecycleMgr.DeleteJob(ctx, js.Name); err != nil {
					log.Printf("WARNING: Could not delete finished job %s: %v", js.Name, err)
				}
			}
		}
	}

	o.processQueue()
	return nil
}

// reconcileLoop runs periodic reconciliation.
func (o *orchestrator) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := o.reconcile(ctx); err != nil {
				log.Printf("Reconcile error: %v", err)
			}
		}
	}
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

// saveState persists the current orchestrator state to the ConfigMap.
func (o *orchestrator) saveState(ctx context.Context) {
	agents := o.identityMgr.ListAgents()
	agentsCopy := make([]identity.AgentIdentity, len(agents))
	for i, a := range agents {
		agentsCopy[i] = *a
	}

	orchState := &state.OrchestratorState{
		Agents:      agentsCopy,
		Queue:       o.assignEngine.GetQueue(),
		Assignments: o.assignEngine.GetAllAssignments(),
		PRMappings:  o.prMappings,
	}

	if err := o.stateMgr.Save(ctx, orchState); err != nil {
		log.Printf("ERROR: Failed to save state: %v", err)
	}
}

// generateSecret creates a random hex-encoded secret for webhook HMAC verification.
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
