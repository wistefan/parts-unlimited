// Package main is the entrypoint for the orchestrator service.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	cfg           *config.Config
	taigaClient   *taiga.Client
	giteaClient   *gitea.Client
	assignEngine  *assignment.Engine
	lifecycleMgr  *lifecycle.Manager
	identityMgr   *identity.Manager
	stateMgr      *state.Manager
	notifySvc     *notifications.Service
	webhookHandler *webhooks.Handler

	projectID     int
	readyStatusID int
	inProgressID  int
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
	log.Printf("  Taiga: statuses resolved (ready=%d, in_progress=%d)", readyStatusID, inProgressID)

	// Resolve role for agent memberships (pick the one with the fewest permissions)
	roles, err := taigaClient.ListRoles(project.ID)
	if err != nil {
		return nil, fmt.Errorf("listing taiga roles: %w", err)
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("no roles found for taiga project")
	}
	agentRoleID := roles[0].ID
	for _, r := range roles {
		if len(r.Permissions) < len(roles[0].Permissions) {
			agentRoleID = r.ID
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

	webhookHandler := webhooks.NewHandler(cfg.Taiga.WebhookSecret)

	// Restore state from ConfigMap
	savedState, err := stateMgr.Load(ctx)
	if err != nil {
		log.Printf("WARNING: Could not load saved state: %v", err)
	}
	if savedState != nil {
		for _, agent := range savedState.Agents {
			identityMgr.RegisterExisting(&agent)
		}
		log.Printf("  State: restored %d agents", len(savedState.Agents))
	}

	orch := &orchestrator{
		cfg:            cfg,
		taigaClient:    taigaClient,
		giteaClient:    giteaClient,
		assignEngine:   assignEngine,
		lifecycleMgr:   lifecycleMgr,
		identityMgr:    identityMgr,
		stateMgr:       stateMgr,
		notifySvc:      notifySvc,
		webhookHandler: webhookHandler,
		projectID:      project.ID,
		readyStatusID:  readyStatusID,
		inProgressID:   inProgressID,
	}

	log.Println("Initialization complete.")
	return orch, nil
}

// registerWebhookHandlers sets up event handlers for Taiga webhook events.
func (o *orchestrator) registerWebhookHandlers() {
	// User story status changed — check if moved to "ready"
	o.webhookHandler.OnFunc("userstory", "change", func(event *webhooks.WebhookEvent) error {
		statusChange, err := webhooks.ParseStatusChange(event)
		if err != nil {
			return err
		}
		if statusChange == nil {
			// Not a status change — could be a tag change, check for delegation
			return o.handleTagChange(event)
		}

		if strings.EqualFold(statusChange.To, StatusReady) {
			data, err := webhooks.ParseUserStoryData(event)
			if err != nil {
				return err
			}
			log.Printf("Ticket #%d moved to ready, enqueuing", data.ID)
			o.assignEngine.Enqueue(data.ID)
			o.processQueue()
		}
		return nil
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

// assignTicket creates an agent and spawns a Job for the given ticket.
func (o *orchestrator) assignTicket(ticketID int) error {
	// Get or create an agent
	agent, err := o.identityMgr.GetOrCreateAgent("general", o.assignEngine.GetBusyAgents())
	if err != nil {
		return fmt.Errorf("getting agent: %w", err)
	}

	// Record the assignment
	o.assignEngine.AssignAgent(ticketID, agent.ID)

	// Move ticket to "in progress"
	story, err := o.taigaClient.GetUserStory(ticketID)
	if err != nil {
		log.Printf("WARNING: Could not fetch ticket %d for status update: %v", ticketID, err)
	} else {
		_, err = o.taigaClient.UpdateUserStory(ticketID, &taiga.UserStoryUpdate{
			Status:     &o.inProgressID,
			AssignedTo: &agent.TaigaUserID,
			Version:    story.Version,
		})
		if err != nil {
			log.Printf("WARNING: Could not update ticket %d status: %v", ticketID, err)
		}
	}

	// Spawn the agent Job
	spec := &lifecycle.AgentJobSpec{
		AgentID:        agent.ID,
		Specialization: agent.Specialization,
		TicketID:       ticketID,
		GiteaUsername:   agent.ID,
		GiteaPassword:   agent.Password,
		TaigaUsername:   agent.ID,
		TaigaPassword:   agent.Password,
		HumanUsername:  o.cfg.Taiga.HumanUsername,
		TaigaProjectID: o.projectID,
	}

	jobName, err := o.lifecycleMgr.CreateJob(context.Background(), spec)
	if err != nil {
		return fmt.Errorf("creating job: %w", err)
	}

	log.Printf("Ticket #%d assigned to %s (job: %s)", ticketID, agent.ID, jobName)

	o.notifySvc.Notify(
		notifications.EventPRReadyForReview,
		ticketID, agent.ID,
		fmt.Sprintf("Ticket #%d assigned", ticketID),
		fmt.Sprintf("Agent %s is working on ticket #%d", agent.ID, ticketID),
		"",
	)

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
	stories, err := o.taigaClient.ListUserStories(o.projectID, &taiga.UserStoryListOptions{
		StatusID: o.readyStatusID,
	})
	if err != nil {
		return fmt.Errorf("listing ready tickets: %w", err)
	}

	for _, story := range stories {
		existing := o.assignEngine.GetAssignment(story.ID)
		if existing == nil {
			log.Printf("Reconcile: found unassigned ready ticket #%d: %s", story.ID, story.Subject)
			o.assignEngine.Enqueue(story.ID)
		}
	}

	// Also check for completed Jobs and free agents
	jobStatuses, err := o.lifecycleMgr.ListActiveJobs(ctx)
	if err != nil {
		log.Printf("WARNING: Could not list active jobs: %v", err)
	} else {
		for _, js := range jobStatuses {
			if js.Succeeded || js.Failed {
				ticketID, _ := strconv.Atoi(js.TicketID)
				if js.Succeeded {
					log.Printf("Reconcile: job %s completed for ticket #%d", js.Name, ticketID)
				} else {
					log.Printf("Reconcile: job %s failed for ticket #%d", js.Name, ticketID)
				}
				o.assignEngine.CompleteTicket(ticketID)
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

	// Check if already registered
	existing, err := o.taigaClient.ListWebhooks(o.projectID)
	if err != nil {
		return fmt.Errorf("listing webhooks: %w", err)
	}
	for _, wh := range existing {
		if wh.Name == WebhookName {
			log.Printf("Taiga webhook already registered (ID: %d)", wh.ID)
			return nil
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
