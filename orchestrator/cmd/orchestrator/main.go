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
	"regexp"
	"strconv"
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

	// reconcilerSvc is the stateless Taiga+Gitea-derived reconciler.
	// Nil when reconcilerMode == "" (legacy-only).
	reconcilerSvc *reconciler.Reconciler

	// reconcilerMode caches config.Agents.ReconcilerMode as the
	// reconciler's typed Mode enum. Compared against ModeAuthoritative
	// to decide whether legacy spawn paths run.
	reconcilerMode reconciler.Mode

	// legacyReconcileActive is true iff the legacy reconcile loop and
	// webhook-driven spawn paths are authoritative. False when the new
	// reconciler has taken over. Distinct from reconcilerMode so future
	// modes (e.g. partial takeovers) can flip independently.
	legacyReconcileActive bool
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

	// Reconcile state on startup (catch missed events). Only the
	// authoritative loop runs the startup pass — in authoritative mode
	// the stateless reconciler does its own initial pass inside
	// runReconcilerLoop.
	if orch.legacyReconcileActive {
		if err := orch.reconcile(ctx); err != nil {
			log.Printf("WARNING: Initial reconciliation failed: %v", err)
		}
	}

	// Queue depth is read from the assignment engine at scrape time so
	// metrics never drift from the live queue state.
	metrics.RegisterQueueDepth(orch.assignEngine.QueueLength)

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

	// Start the legacy reconcile loop only when it is still the
	// authoritative spawn path. In authoritative mode the new
	// reconciler is the single writer; running both would race.
	if orch.legacyReconcileActive {
		go orch.reconcileLoop(ctx)
	}

	// Start the stateless reconciler when enabled (shadow or
	// authoritative mode). Shadow runs alongside the legacy loop and
	// only logs; authoritative replaces the legacy loop and actually
	// spawns.
	if orch.reconcilerSvc != nil {
		go orch.runReconcilerLoop(ctx, orch.reconcilerSvc)
		log.Printf("Reconciler enabled in %q mode (interval: %s)",
			cfg.Agents.ReconcilerMode, ReconcileInterval)
	}

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
		// Restore ticket assignments with their original status so that
		// "waiting" assignments stay waiting (not re-spawned).
		if savedState.Assignments != nil {
			for _, a := range savedState.Assignments {
				assignEngine.RestoreAssignment(a)
			}
		}
		log.Printf("  State: restored %d agents, %d assignments, %d PR mappings",
			len(savedState.Agents), len(savedState.Assignments), len(prMappings))
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

	// Configure the stateless reconciler. The Spawner is wired after
	// the orchestrator struct is constructed so it can close over the
	// orchestrator's existing spawn helpers.
	switch cfg.Agents.ReconcilerMode {
	case "authoritative":
		orch.reconcilerMode = reconciler.ModeAuthoritative
		orch.legacyReconcileActive = false
	case "shadow":
		orch.reconcilerMode = reconciler.ModeShadow
		orch.legacyReconcileActive = true
	case "":
		// Legacy-only: no reconciler service is built.
		orch.legacyReconcileActive = true
	default:
		log.Printf("WARNING: unknown reconcilerMode %q, falling back to legacy-only", cfg.Agents.ReconcilerMode)
		orch.legacyReconcileActive = true
	}

	if cfg.Agents.ReconcilerMode == "shadow" || cfg.Agents.ReconcilerMode == "authoritative" {
		orch.reconcilerSvc = reconciler.New(reconciler.Config{
			Taiga:          taigaClient,
			Gitea:          giteaClient,
			Jobs:           lifecycleMgr,
			Spawner:        &orchSpawner{o: orch},
			Log:            log.Default(),
			Mode:           orch.reconcilerMode,
			ProjectID:      project.ID,
			ReadyStatusID:  readyStatusID,
			InProgressID:   inProgressID,
			HumanTaigaID:   humanTaigaID,
			MaxConcurrency: cfg.Agents.MaxConcurrency,
		})
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

		// Ticket moved to "ready" — nudge reconciler (authoritative) or
		// enqueue via legacy path.
		if strings.EqualFold(statusName, StatusReady) {
			if !o.legacyReconcileActive {
				log.Printf("Ticket #%d: status is ready, nudging reconciler", data.ID)
				o.nudgeReconciler(data.ID)
				return nil
			}
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
			if !o.legacyReconcileActive {
				log.Printf("Ticket #%d: unassigned, nudging reconciler", data.ID)
				o.nudgeReconciler(data.ID)
				return nil
			}
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
			if !o.legacyReconcileActive {
				log.Printf("New ticket #%d created in ready status, nudging reconciler", data.ID)
				o.nudgeReconciler(data.ID)
				return nil
			}
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

		// Guard against duplicate webhooks: if an agent is already actively
		// assigned (not just "waiting"), another webhook already handled
		// this merge event.
		if existing := o.assignEngine.GetAssignment(ticketID); existing != nil && existing.Status == "assigned" {
			log.Printf("Gitea: ticket #%d already has active assignment (%s), skipping duplicate merge event", ticketID, existing.PrimaryAgent)
			return nil
		}

		// Clear the "waiting" assignment before spawning the next agent
		o.assignEngine.CompleteTicket(ticketID)

		// Check the agent's last Taiga comment for step markers
		if o.isStepComplete(ticketID) {
			log.Printf("Gitea: ticket #%d all steps complete — transitioning to ready-for-test", ticketID)
			o.transitionToReadyForTest(ticketID)
			return nil
		}

		if !o.legacyReconcileActive {
			log.Printf("Gitea: ticket #%d PR merged — nudging reconciler", ticketID)
			o.nudgeReconciler(ticketID)
			return nil
		}

		log.Printf("Gitea: ticket #%d PR merged — spawning step agent", ticketID)
		o.spawnAgentForTicket(ticketID, "step")
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

		// Guard against duplicate webhooks: if a job already exists for
		// this ticket, skip — a previous webhook already handled it.
		if hasJob, _ := o.lifecycleMgr.HasJobForTicket(context.Background(), ticketID); hasJob {
			log.Printf("Gitea: ticket #%d already has a running job, skipping duplicate review event", ticketID)
			return nil
		}

		if !o.legacyReconcileActive {
			// In authoritative mode the reconciler detects the
			// non-stale REQUEST_CHANGES review via Gitea and spawns
			// the fix agent itself — no direct spawn from the webhook.
			o.nudgeReconciler(ticketID)
			return nil
		}

		// Try to reuse the existing agent; if none is assigned (e.g. after
		// restart or PR-close cleared it), get or create one.
		var agent *identity.AgentIdentity
		existing := o.assignEngine.GetAssignment(ticketID)
		if existing != nil {
			o.assignEngine.AssignAgent(ticketID, existing.PrimaryAgent)
			agent = o.identityMgr.GetAgent(existing.PrimaryAgent)
		}
		if agent == nil {
			var err error
			agent, err = o.identityMgr.GetOrCreateAgent("general", o.assignEngine.GetBusyAgents())
			if err != nil {
				log.Printf("ERROR: could not get agent for fix on ticket %d: %v", ticketID, err)
				return nil
			}
			o.assignEngine.AssignAgent(ticketID, agent.ID)
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
			HumanTaigaID:   o.humanTaigaID,
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

// extractModeFromJobName extracts the mode suffix from a Job name.
// Job names follow the pattern: agent-{id}-ticket-{id}-{mode}
func extractModeFromJobName(jobName string) string {
	lastDash := strings.LastIndex(jobName, "-")
	if lastDash < 0 {
		return "step"
	}
	return jobName[lastDash+1:]
}

// determineMode derives the agent mode from the ticket's Taiga comment history.
// It scans all comments to build a complete lifecycle picture rather than
// relying on the single most-recent marker (which can be stale or duplicated).
func (o *orchestrator) determineMode(ticketID int) string {
	comments, err := o.taigaClient.GetComments(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch comments for ticket %d to determine mode: %v", ticketID, err)
		return "analysis"
	}

	// Collect which lifecycle markers exist anywhere in the history.
	hasAnalysisProceed := false
	hasAnalysisNeedInfo := false
	hasAnalysisOneStepProceed := false
	hasAnalysisOneStepRejected := false
	hasPlanCreated := false
	hasStep := false
	hasStepComplete := false
	hasFixApplied := false

	// Also find the newest marker of each type (comments are newest-first).
	newestMarker := ""
	for _, entry := range comments {
		c := entry.Comment
		if strings.Contains(c, "[step:complete]") {
			hasStepComplete = true
			if newestMarker == "" {
				newestMarker = "step:complete"
			}
		}
		if strings.Contains(c, "[fix:applied]") {
			hasFixApplied = true
			if newestMarker == "" {
				newestMarker = "fix:applied"
			}
		}
		if strings.Contains(c, "[step:") && !strings.Contains(c, "[step:complete]") {
			hasStep = true
			if newestMarker == "" {
				newestMarker = "step"
			}
		}
		if strings.Contains(c, "[phase:plan-created]") {
			hasPlanCreated = true
			if newestMarker == "" {
				newestMarker = "phase:plan-created"
			}
		}
		if strings.Contains(c, "[analysis:onestep-proceed]") {
			hasAnalysisOneStepProceed = true
			if newestMarker == "" {
				newestMarker = "analysis:onestep-proceed"
			}
		}
		if strings.Contains(c, "[analysis:onestep-rejected]") {
			hasAnalysisOneStepRejected = true
			if newestMarker == "" {
				newestMarker = "analysis:onestep-rejected"
			}
		}
		// [analysis:proceed] is a proper substring check that must not
		// match the onestep variants (both contain "analysis:" but not
		// literal "analysis:proceed]" / "analysis:need-info]").
		if strings.Contains(c, "[analysis:proceed]") {
			hasAnalysisProceed = true
			if newestMarker == "" {
				newestMarker = "analysis:proceed"
			}
		}
		if strings.Contains(c, "[analysis:need-info]") {
			hasAnalysisNeedInfo = true
			if newestMarker == "" {
				newestMarker = "analysis:need-info"
			}
		}
	}

	// Decision logic using the full picture:
	//
	// 1. If steps were ever started, the plan was already merged — ignore
	//    stale [phase:plan-created] markers from duplicate agent runs.
	// 2. If [step:complete] exists, all implementation is done (covers both
	//    multi-step finish and onestep finish).
	// 3. The newest marker determines the current waiting state.

	if hasStepComplete {
		return "" // all done (multi-step or onestep)
	}
	if hasStep {
		// Steps are in progress.  The newest marker tells us what to wait for.
		if newestMarker == "fix:applied" || newestMarker == "step" {
			return "" // step or fix PR open, waiting for human
		}
		// If newest marker is a stale plan-created, we're actually in step
		// mode — a step PR was merged or is pending.
		return "step"
	}
	if hasPlanCreated {
		if newestMarker == "fix:applied" {
			return "" // fix pushed for plan PR, waiting for re-review
		}
		// Plan created, no steps yet — waiting for plan PR merge.
		return ""
	}
	// Onestep path: analysis agreed with the one-step tag and a onestep
	// agent should run (or has run and is waiting on a PR — fix:applied on
	// a onestep PR still means "waiting for re-review").
	if hasAnalysisOneStepProceed {
		if newestMarker == "fix:applied" {
			return "" // fix pushed for onestep PR, waiting for re-review
		}
		return "onestep"
	}
	if hasAnalysisOneStepRejected {
		return "" // analysis rejected one-step — ticket is with the human
	}
	if hasAnalysisProceed {
		return "plan"
	}
	if hasAnalysisNeedInfo {
		return ""
	}
	if !hasAnalysisProceed && !hasPlanCreated && !hasStep {
		return "analysis"
	}

	_ = hasFixApplied // used implicitly via newestMarker
	return "analysis"
}

// handleJobCompletion processes the result of a completed agent Job.
// The mode is extracted from the Job name (not stored in the assignment).
func (o *orchestrator) handleJobCompletion(ctx context.Context, ticketID int, mode string, assgn *assignment.TicketAssignment) {
	switch mode {
	case "analysis":
		o.handleAnalysisCompletion(ctx, ticketID, assgn)
	default:
		// For plan/step/fix modes, keep the assignment alive in "waiting"
		// status.  This prevents the reconciliation loop from re-spawning
		// agents while we wait for a Gitea PR event (merge, review).
		// The agent is released from the busy pool so it can work on other
		// tickets.  The Gitea webhook handlers (PR merge, review) will
		// either clear the assignment or reactivate it.
		log.Printf("Ticket #%d: %s Job completed, waiting for PR events", ticketID, mode)
		o.assignEngine.WaitForPR(ticketID)
		o.saveState(ctx)
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

	// Search from newest to oldest for the analysis marker.
	// Taiga history API returns newest-first (index 0 = newest).
	//
	// Check order: onestep variants first, because they are the more
	// specific markers and can coexist with a later [analysis:proceed]
	// from a re-analysis round — we want the onestep decision to win
	// when it's newer.
	analysisResult := ""
	for i := 0; i < len(comments); i++ {
		c := comments[i]
		if strings.Contains(c.Comment, "[analysis:onestep-proceed]") {
			analysisResult = "onestep-proceed"
			break
		}
		if strings.Contains(c.Comment, "[analysis:onestep-rejected]") {
			analysisResult = "onestep-rejected"
			break
		}
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
		o.transitionToInProgress(ticketID)
		o.respawnAgent(ticketID, "plan")

	case "onestep-proceed":
		log.Printf("Ticket #%d: analysis result = onestep-proceed, moving to in-progress and spawning onestep agent", ticketID)
		o.transitionToInProgress(ticketID)
		o.respawnAgent(ticketID, "onestep")

	case "onestep-rejected":
		log.Printf("Ticket #%d: analysis result = onestep-rejected, ticket assigned to human for reevaluation", ticketID)
		// The analysis agent already assigned the ticket to the human and
		// posted the rejection comment.  Clear the internal assignment so
		// the orchestrator picks it up again when the human unassigns.
		o.assignEngine.CompleteTicket(ticketID)

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
// marker. Every agent comment carries the marker forward, so the most
// recent agent comment is sufficient — no need to walk all the way
// back to the first one. Returns ("", "") when no marker exists yet
// (first-agent case): the agent then resolves the repo itself from the
// ticket description.
func (o *orchestrator) findRepoForTicket(ticketID int) (owner, name string) {
	comments, err := o.taigaClient.GetComments(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch comments for ticket %d to resolve repo: %v", ticketID, err)
		return "", ""
	}
	// GetComments returns newest-first; the first match is the latest
	// agent comment. Non-agent comments (human replies, orchestrator-
	// posted escalations) won't contain the marker and are skipped.
	for _, c := range comments {
		if m := repoMarkerRE.FindStringSubmatch(c.Comment); len(m) == 3 {
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

// pickAgentForTicket returns the agent identity to use when spawning
// the next Job for a ticket. Prefers the last agent that commented on
// the ticket so follow-up work lands on the same fork/branch/plan
// context. Falls back to any free general agent when the previous
// agent is unknown to the identity manager or already busy with
// another ticket. Purely Taiga-backed — no orchestrator state needed.
func (o *orchestrator) pickAgentForTicket(ticketID int) (*identity.AgentIdentity, error) {
	if prev := o.findLastAgentForTicket(ticketID); prev != "" {
		if a := o.identityMgr.GetAgent(prev); a != nil {
			if !o.assignEngine.GetBusyAgents()[prev] {
				log.Printf("Ticket #%d: continuing with previous agent %s", ticketID, prev)
				return a, nil
			}
			log.Printf("Ticket #%d: previous agent %s is busy; picking a fresh one", ticketID, prev)
		}
	}
	return o.identityMgr.GetOrCreateAgent("general", o.assignEngine.GetBusyAgents())
}

// spawnAgentForTicket creates (or reuses) an agent and spawns a Job for the given
// ticket and mode.  Unlike respawnAgent, this does not require an existing assignment.
// Used by Gitea webhook handlers where the assignment was already cleared.
func (o *orchestrator) spawnAgentForTicket(ticketID int, mode string) {
	if o.ticketAssignedToHuman(ticketID) {
		log.Printf("Ticket #%d: assigned to human, skipping %s spawn", ticketID, mode)
		return
	}
	agent, err := o.pickAgentForTicket(ticketID)
	if err != nil {
		log.Printf("ERROR: could not get agent for ticket %d: %v", ticketID, err)
		return
	}

	o.assignEngine.AssignAgent(ticketID, agent.ID)
	o.assignTicketInTaiga(ticketID, agent.TaigaUserID)

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
		HumanTaigaID:   o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}
	o.pinRepoToTicket(ticketID, agent.ID, spec)

	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		log.Printf("ERROR: could not spawn %s agent for ticket %d: %v", mode, ticketID, err)
		return
	}

	log.Printf("Ticket #%d: %s agent %s spawned (job: %s)", ticketID, mode, agent.ID, jobName)
	o.saveState(context.Background())
}

// respawnAgent re-spawns the appropriate agent for a ticket with the
// given mode. The agent is derived from Taiga comment history (via
// pickAgentForTicket) rather than in-memory assignment state so that
// the orchestrator stays stateless and survives restarts without
// losing track of which agent owns which ticket.
func (o *orchestrator) respawnAgent(ticketID int, mode string) {
	if o.ticketAssignedToHuman(ticketID) {
		log.Printf("Ticket #%d: assigned to human, skipping %s respawn", ticketID, mode)
		return
	}
	agent, err := o.pickAgentForTicket(ticketID)
	if err != nil {
		log.Printf("ERROR: Could not get agent for ticket %d: %v", ticketID, err)
		return
	}

	// Keep the engine's in-memory assignment in sync when the Taiga-
	// derived agent differs from whatever was previously cached.
	o.assignEngine.AssignAgent(ticketID, agent.ID)
	o.assignTicketInTaiga(ticketID, agent.TaigaUserID)

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
		HumanTaigaID:   o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}
	o.pinRepoToTicket(ticketID, agent.ID, spec)

	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		log.Printf("ERROR: Could not re-spawn agent for ticket %d: %v", ticketID, err)
		return
	}

	log.Printf("Ticket #%d: agent %s re-spawned in %s mode (job: %s)", ticketID, agent.ID, mode, jobName)
	o.saveState(context.Background())
}

// prNeedingFix holds info about a PR that needs a fix agent.
type prNeedingFix struct {
	repo   string // "owner/repo"
	number int
}

// findPRNeedingFix checks Gitea for open PRs associated with a ticket that
// have non-stale REQUEST_CHANGES reviews.  Returns the first such PR, or nil.
// This catches review events the orchestrator missed (e.g. during a restart).
//
// PRs are discovered via ListPullRequestsForTicket (branch-prefix scan on the
// ticket's canonical repo), not via the in-memory prMappings — Gitea is the
// single source of truth and survives orchestrator restarts without any
// persisted state.
func (o *orchestrator) findPRNeedingFix(ticketID int) *prNeedingFix {
	owner, name := o.findRepoForTicket(ticketID)
	if owner == "" || name == "" {
		return nil
	}
	prs, err := o.giteaClient.ListPullRequestsForTicket(owner, name, ticketID)
	if err != nil {
		log.Printf("WARNING: findPRNeedingFix: listing PRs for ticket %d: %v", ticketID, err)
		return nil
	}
	for _, pr := range prs {
		if pr.State != "open" {
			continue
		}
		reviews, err := o.giteaClient.GetPRReviews(owner, name, pr.Number)
		if err != nil {
			continue
		}
		for _, r := range reviews {
			if r.State == "REQUEST_CHANGES" && !r.Stale {
				return &prNeedingFix{repo: owner + "/" + name, number: pr.Number}
			}
		}
	}
	return nil
}

// determineModeFromPRState checks Gitea for merged PRs associated with a ticket
// and returns the next agent mode that should be spawned.  This recovers from
// events the orchestrator missed (e.g. a plan PR merged while the orchestrator
// was down).  Returns "" if no follow-up is needed.
//
// As with findPRNeedingFix, PRs are discovered via branch-prefix scan on the
// ticket's canonical repo, not via prMappings.
func (o *orchestrator) determineModeFromPRState(ticketID int) string {
	owner, name := o.findRepoForTicket(ticketID)
	if owner == "" || name == "" {
		return ""
	}
	prs, err := o.giteaClient.ListPullRequestsForTicket(owner, name, ticketID)
	if err != nil {
		log.Printf("WARNING: determineModeFromPRState: listing PRs for ticket %d: %v", ticketID, err)
		return ""
	}

	hasMergedPlanPR := false
	hasMergedStepPR := false
	hasOpenPR := false

	for _, pr := range prs {
		if pr.State == "open" {
			hasOpenPR = true
			continue
		}
		if !pr.Merged {
			continue
		}
		if strings.Contains(pr.Title, "Implementation Plan") || strings.Contains(pr.Title, "implementation plan") {
			hasMergedPlanPR = true
		} else {
			hasMergedStepPR = true
		}
	}

	if hasOpenPR {
		return "" // still waiting for review/merge
	}
	if hasMergedStepPR {
		// A step PR was merged — check if all steps are done (via comments).
		if o.isStepComplete(ticketID) {
			return ""
		}
		return "step"
	}
	if hasMergedPlanPR {
		return "step" // plan merged, need to start step work
	}
	return ""
}

// spawnFixAgent creates a fix agent for a specific PR on a ticket.
// The agent is resolved from Taiga comment history so the agent that
// authored the PR is the one asked to fix it — they already have the
// workspace/plan context and own the branch.
func (o *orchestrator) spawnFixAgent(ticketID int, repoFullName, repoOwner, repoName string, prNumber int) {
	if o.ticketAssignedToHuman(ticketID) {
		log.Printf("Ticket #%d: assigned to human, skipping fix spawn", ticketID)
		return
	}
	agent, err := o.pickAgentForTicket(ticketID)
	if err != nil {
		log.Printf("ERROR: could not get agent for fix on ticket %d: %v", ticketID, err)
		return
	}
	o.assignEngine.AssignAgent(ticketID, agent.ID)
	o.assignTicketInTaiga(ticketID, agent.TaigaUserID)

	spec := &lifecycle.AgentJobSpec{
		AgentID:        agent.ID,
		Specialization: agent.Specialization,
		TicketID:       ticketID,
		Mode:           "fix",
		PRNumber:       prNumber,
		PRRepo:         repoFullName,
		RepoOwner:      repoOwner,
		RepoName:       repoName,
		GiteaUsername:  agent.ID,
		GiteaPassword:  agent.Password,
		TaigaUsername:  agent.ID,
		TaigaPassword:  agent.Password,
		HumanUsername:  o.cfg.Taiga.HumanUsername,
		HumanTaigaID:   o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}
	// Fix agents operate on the PR's existing repo. Grant write access
	// when the agent isn't the owner so the fix branch can be pushed.
	if repoOwner != "" && repoOwner != agent.ID {
		if err := o.giteaClient.AddCollaborator(repoOwner, repoName, agent.ID, "write"); err != nil {
			log.Printf("WARNING: ticket #%d: could not grant fix agent %s write access to %s/%s: %v",
				ticketID, agent.ID, repoOwner, repoName, err)
		}
	}
	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		log.Printf("ERROR: could not spawn fix agent for ticket %d: %v", ticketID, err)
		return
	}
	log.Printf("Ticket #%d: fix agent %s spawned (job: %s) for PR %s#%d",
		ticketID, agent.ID, jobName, repoFullName, prNumber)
	o.saveState(context.Background())
}

// postLifecycleMarker posts a lifecycle comment on a Taiga ticket so that
// determineMode() knows the ticket is waiting for a PR event.  The marker
// is mode-specific and tells the orchestrator not to re-spawn an agent.
// If the agent already posted the marker, the duplicate is harmless — the
// newest occurrence wins in the comment search.
func (o *orchestrator) postLifecycleMarker(ticketID int, mode string) {
	var marker string
	switch mode {
	case "plan":
		marker = "[phase:plan-created]"
	case "step":
		// Use a generic step marker; the agent may have posted a more
		// specific [step:N/M] or [step:complete] already.
		marker = "[step:in-progress]"
	case "fix":
		marker = "[fix:applied]"
	default:
		return
	}

	// Check whether an equivalent marker is already present (avoid noisy duplicates).
	// For step mode, any [step:*] marker suffices (e.g. [step:3/7]).
	comments, err := o.taigaClient.GetComments(ticketID)
	if err == nil {
		checkPrefix := marker
		if mode == "step" {
			checkPrefix = "[step:"
		}
		for i := 0; i < len(comments); i++ {
			if strings.Contains(comments[i].Comment, checkPrefix) {
				return // already posted
			}
			// Stop searching once we hit [analysis:proceed] — only check
			// comments that are newer than the last mode transition.
			if strings.Contains(comments[i].Comment, "[analysis:proceed]") {
				break
			}
		}
	}

	story, err := o.taigaClient.GetUserStory(ticketID)
	if err != nil {
		log.Printf("WARNING: could not fetch ticket %d to post lifecycle marker: %v", ticketID, err)
		return
	}

	comment := fmt.Sprintf("%s\n\nAgent job completed — waiting for PR review/merge.", marker)
	_, err = o.taigaClient.UpdateUserStory(ticketID, &taiga.UserStoryUpdate{
		Comment: comment,
		Version: story.Version,
	})
	if err != nil {
		log.Printf("WARNING: could not post lifecycle marker on ticket %d: %v", ticketID, err)
	}
}

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
	if o.ticketAssignedToHuman(ticketID) {
		log.Printf("Ticket #%d: assigned to human, dropping from queue", ticketID)
		return nil
	}
	// Get or create an agent
	agent, err := o.identityMgr.GetOrCreateAgent("general", o.assignEngine.GetBusyAgents())
	if err != nil {
		return fmt.Errorf("getting agent: %w", err)
	}

	// Record the assignment internally — ticket status stays "ready" in Taiga
	o.assignEngine.AssignAgent(ticketID, agent.ID)
	o.assignTicketInTaiga(ticketID, agent.TaigaUserID)

	// Derive the mode from ticket state (comments) — always start fresh
	mode := o.determineMode(ticketID)
	if mode == "" {
		log.Printf("Ticket #%d: no work needed (complete or waiting for human)", ticketID)
		o.assignEngine.CompleteTicket(ticketID)
		return nil
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
		HumanTaigaID:   o.humanTaigaID,
		TaigaProjectID: o.projectID,
	}

	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		return fmt.Errorf("creating job: %w", err)
	}

	log.Printf("Ticket #%d: %s assigned to %s (job: %s)", ticketID, mode, agent.ID, jobName)

	// Save state
	o.saveState(context.Background())

	return nil
}

// handleDelegation creates a specialized agent for delegated work.
func (o *orchestrator) handleDelegation(ticketID int, specialization string) {
	if o.ticketAssignedToHuman(ticketID) {
		log.Printf("Ticket #%d: assigned to human, skipping delegation to %s", ticketID, specialization)
		return
	}
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

	// Check all tracked assignments (both "assigned" and "waiting").
	// After an orchestrator restart, events may have occurred while we were
	// down (PRs merged, tickets closed, etc.) so we re-validate everything.
	for ticketID, a := range o.assignEngine.GetAllAssignments() {
		if a.Status != "assigned" && a.Status != "waiting" {
			continue
		}

		// Validate the ticket still exists and is in a workable status.
		story, err := o.taigaClient.GetUserStory(ticketID)
		if err != nil {
			log.Printf("Reconcile: ticket #%d no longer accessible, clearing assignment: %v", ticketID, err)
			o.assignEngine.CompleteTicket(ticketID)
			continue
		}
		if story.Status != o.readyStatusID && story.Status != o.inProgressID {
			log.Printf("Reconcile: ticket #%d is not in ready/in-progress status, clearing assignment", ticketID)
			o.assignEngine.CompleteTicket(ticketID)
			continue
		}

		// Never touch tickets the human currently owns — they're blocked
		// on human input, and any spawn / respawn / comment would both
		// be unwanted and spam the ticket history.
		if o.humanTaigaID > 0 && story.AssignedTo != nil && *story.AssignedTo == o.humanTaigaID {
			continue
		}

		// Check whether ANY job exists for this ticket (regardless of mode).
		hasJob, err := o.lifecycleMgr.HasJobForTicket(ctx, ticketID)
		if err != nil {
			log.Printf("WARNING: could not check jobs for ticket %d: %v", ticketID, err)
			continue
		}
		if hasJob {
			continue
		}

		// Derive the correct mode from the ticket's comment history.
		mode := o.determineMode(ticketID)
		if mode == "" {
			// Comments say "waiting for PR" or "done".  Check Gitea for
			// open PRs with pending review-requested-changes that the
			// orchestrator may have missed (e.g. during a restart).
			if pr := o.findPRNeedingFix(ticketID); pr != nil {
				parts := strings.SplitN(pr.repo, "/", 2)
				log.Printf("Reconcile: ticket #%d has open PR %s#%d with requested changes, spawning fix agent",
					ticketID, pr.repo, pr.number)
				o.spawnFixAgent(ticketID, pr.repo, parts[0], parts[1], pr.number)
				continue
			}
			// Check if a PR was merged while the orchestrator was down
			// (e.g. plan PR merged → need step agent).
			if recoveredMode := o.determineModeFromPRState(ticketID); recoveredMode != "" {
				log.Printf("Reconcile: ticket #%d has merged PR requiring follow-up, spawning %s agent", ticketID, recoveredMode)
				mode = recoveredMode
			} else {
				if a.Status == "assigned" {
					log.Printf("Reconcile: ticket #%d is complete or waiting for PR, clearing assignment", ticketID)
					o.assignEngine.CompleteTicket(ticketID)
				}
				continue
			}
		}

		log.Printf("Reconcile: ticket #%d has no Job, derived mode=%s, spawning", ticketID, mode)
		o.respawnAgent(ticketID, mode)
	}

	// Check for "in progress" tickets that have no running Job and are
	// not assigned to the human — these may need an agent re-spawned.
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
			if story.AssignedTo != nil && *story.AssignedTo == o.humanTaigaID {
				continue
			}
			// Check if a Job already exists for this ticket (e.g. spawned by
			// a webhook handler but not yet tracked in the assignment engine).
			hasJob, jobErr := o.lifecycleMgr.HasJobForTicket(ctx, story.ID)
			if jobErr != nil {
				log.Printf("WARNING: could not check jobs for ticket %d: %v", story.ID, jobErr)
				continue
			}
			if hasJob {
				continue
			}
			// Derive the correct mode from the ticket's comment history.
			mode := o.determineMode(story.ID)
			if mode == "" {
				// Comments say "waiting" or "done" — check Gitea for open
				// PRs with pending review-requested-changes that were missed.
				if pr := o.findPRNeedingFix(story.ID); pr != nil {
					parts := strings.SplitN(pr.repo, "/", 2)
					log.Printf("Reconcile: in-progress ticket #%d has open PR %s#%d with requested changes, spawning fix agent",
						story.ID, pr.repo, pr.number)
					o.spawnFixAgent(story.ID, pr.repo, parts[0], parts[1], pr.number)
					continue
				}
				// Check if a PR was merged while the orchestrator was down.
				if recoveredMode := o.determineModeFromPRState(story.ID); recoveredMode != "" {
					log.Printf("Reconcile: in-progress ticket #%d has merged PR requiring follow-up, spawning %s agent", story.ID, recoveredMode)
					mode = recoveredMode
				} else {
					continue
				}
			}
			log.Printf("Reconcile: in-progress ticket #%d has no agent, re-spawning in %s mode", story.ID, mode)
			o.respawnAgent(story.ID, mode)
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
					// Extract mode from job name (format: agent-{id}-ticket-{id}-{mode})
					jobMode := extractModeFromJobName(js.Name)
					if js.Succeeded {
						log.Printf("Reconcile: job %s completed for ticket #%d (mode=%s)", js.Name, ticketID, jobMode)
						o.handleJobCompletion(ctx, ticketID, jobMode, assignment)
					} else {
						log.Printf("Reconcile: job %s failed for ticket #%d", js.Name, ticketID)
						o.assignEngine.CompleteTicket(ticketID)
					}
				}
				// Delete finished Jobs (safe — each mode has a unique Job name)
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

// runReconcilerLoop runs the stateless reconciler on the same interval
// as the legacy loop. In shadow mode it only logs, so it is safe to
// run alongside the legacy loop. In authoritative mode the legacy loop
// is disabled and this loop is the single spawn path.
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
	if err != nil {
		return fmt.Errorf("picking agent: %w", err)
	}

	// The legacy engine map and saved state are still populated for
	// other code paths that read them (removed in step 6). Nothing
	// authoritative depends on this write any more.
	o.assignEngine.AssignAgent(ticketID, agent.ID)
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
	o.saveState(ctx)
	return nil
}

// pickAgentForTicketWithBusy is the explicit-busy-set variant of
// pickAgentForTicket. The reconciler passes a K8s-derived busy-agents
// map so no in-memory engine state is read on the authoritative path.
func (o *orchestrator) pickAgentForTicketWithBusy(ticketID int, busy map[string]bool) (*identity.AgentIdentity, error) {
	if prev := o.findLastAgentForTicket(ticketID); prev != "" {
		if a := o.identityMgr.GetAgent(prev); a != nil {
			if !busy[prev] {
				log.Printf("Ticket #%d: continuing with previous agent %s", ticketID, prev)
				return a, nil
			}
			log.Printf("Ticket #%d: previous agent %s is busy; picking a fresh one", ticketID, prev)
		}
	}
	return o.identityMgr.GetOrCreateAgent("general", busy)
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
