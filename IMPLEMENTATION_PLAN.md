# Implementation Plan: Dev-Env — AI Agent Development Orchestration

## Architecture Overview

### System Context

The system is a locally-hosted, Kubernetes-based platform that orchestrates Claude AI agents to autonomously perform software development work driven by tickets from Taiga. All components run on a single machine using k3s (lightweight Kubernetes).

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        Host Machine (k3s)                               │
│                                                                         │
│  ┌──────────────┐   ┌──────────────┐   ┌────────────────────────────┐  │
│  │   Namespace:  │   │   Namespace:  │   │     Namespace: agents      │  │
│  │    gitea      │   │    taiga      │   │                            │  │
│  │              │   │              │   │  ┌──────────────────────┐  │  │
│  │  ┌────────┐  │   │  ┌────────┐  │   │  │    Orchestrator      │  │  │
│  │  │ Gitea  │  │   │  │ Taiga  │  │   │  │    (Deployment)      │  │  │
│  │  │ Server │  │   │  │ Back   │  │   │  │                      │  │  │
│  │  └────────┘  │   │  └────────┘  │   │  │  - Webhook listener  │  │  │
│  │  ┌────────┐  │   │  ┌────────┐  │   │  │  - Assignment engine │  │  │
│  │  │Postgres│  │   │  │ Taiga  │  │   │  │  - Agent lifecycle   │  │  │
│  │  └────────┘  │   │  │ Front  │  │   │  │  - PR reviewer       │  │  │
│  │  ┌────────┐  │   │  └────────┘  │   │  │  - State manager     │  │  │
│  │  │  Act   │  │   │  ┌────────┐  │   │  └──────────┬───────────┘  │  │
│  │  │ Runner │  │   │  │Postgres│  │   │             │              │  │
│  │  └────────┘  │   │  └────────┘  │   │             │ creates      │  │
│  └──────────────┘   │  ┌────────┐  │   │             ▼              │  │
│                     │  │RabbitMQ│  │   │  ┌──────────────────────┐  │  │
│                     │  └────────┘  │   │  │   Agent Worker Jobs   │  │  │
│                     └──────────────┘   │  │                      │  │  │
│                                        │  │  ┌─────┐ ┌─────┐    │  │  │
│                                        │  │  │Job 1│ │Job 2│ .. │  │  │
│                                        │  │  └─────┘ └─────┘    │  │  │
│                                        │  └──────────────────────┘  │  │
│                                        │  ┌──────────────────────┐  │  │
│                                        │  │   ConfigMap/Secrets   │  │  │
│                                        │  │  - Service endpoints  │  │  │
│                                        │  │  - API keys           │  │  │
│                                        │  │  - Agent registry     │  │  │
│                                        │  └──────────────────────┘  │  │
│                                        └────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────┘
```

### Component Details

#### Orchestrator (Go Service, Kubernetes Deployment)

The orchestrator is a **traditional Go service** (not a Claude AI instance) that invokes Claude instances as workers. This design separates deterministic control logic (assignment, scheduling, lifecycle management) from the AI's creative coding work, making the system more predictable and debuggable.

It runs as a long-lived Kubernetes Deployment (replicas: 1) with the following responsibilities:

| Subsystem | Responsibility |
|---|---|
| **Webhook Listener** | HTTP server receiving Taiga webhook events. Processes ticket state changes, new comments, and assignment updates. Falls back to polling Taiga API on startup to catch events missed while down. |
| **Assignment Engine** | Maintains a FIFO queue of "ready" tickets. Assigns tickets to available agents respecting max concurrency. Handles tag-based delegation by detecting delegation tags and assigning to available specialized agents. |
| **Agent Lifecycle Manager** | Creates and destroys Kubernetes Jobs for agent workers. Tracks active agents, their current tickets, and container status. Destroys idle containers after a configurable timeout (default: 5 minutes). |
| **PR Review Service** | Watches Gitea for new PRs from agent branches. Invokes Claude Code CLI (`claude -p`) as a subprocess to review the PR diff and post review comments on Gitea. Does not approve or merge. |
| **State Manager** | Persists orchestrator state (agent registry, ticket assignments, FIFO queue) to a Kubernetes ConfigMap. On startup, reconstructs state from this ConfigMap and reconciles with Taiga/Gitea actual state. |
| **Notification Service** | Sends notifications to the human for escalations, quota warnings, and tickets needing input. Uses a local webhook endpoint (configurable) that can integrate with desktop notifications, a local web dashboard, or email via a local SMTP relay. |

**Technology choice:** Go 1.22+ with:
- `net/http` (or `chi` router) for the webhook HTTP server
- `claude -p` CLI subprocess invocation for spawning Claude coding agents and PR reviews
- `k8s.io/client-go` (official Kubernetes client, same library K8s itself uses) for managing Jobs
- `net/http` for Taiga and Gitea API clients
- Go structs with JSON serialization for configuration and state models

**Why Go over Python:**
- Lower memory footprint and faster startup — important for a long-running orchestrator service
- Superior concurrency model (goroutines) for managing multiple agent lifecycles in parallel
- `client-go` is the canonical Kubernetes client library, used by K8s itself
- Compiles to a single static binary — simpler container image (scratch/distroless base, ~20 MB)
- No official Claude Agent SDK for Go exists, but the orchestrator only needs `claude -p` subprocess calls, which are straightforward in Go via `os/exec`

#### Agent Workers (Claude Code in Kubernetes Jobs)

Each agent worker is a Kubernetes Job running a container with Claude Code installed. The container image includes:
- Node.js 22 (for Claude Code CLI)
- Git, common build tools
- Language-specific toolchains are installed by the agent at runtime based on the project needs

Agent containers run with `--dangerously-skip-permissions` since they are sandboxed inside Kubernetes with constrained network policies. Each agent:
1. Receives its task via environment variables (ticket ID, Taiga/Gitea URLs, agent credentials)
2. Reads the ticket from Taiga, checks the implementation plan
3. Clones the repo from Gitea, creates a branch
4. Performs the coding work autonomously
5. Runs tests and linters
6. Creates a PR on Gitea with a link to the ticket and the plan step
7. Updates the ticket status/assignment as needed
8. Exits (Job completes, container destroyed after TTL)

```
┌─────────────────────────────────────────────────────┐
│                  Agent Worker Container               │
│                                                       │
│  ┌──────────────────────────────────────────────┐    │
│  │              Claude Code CLI                   │    │
│  │  claude -p --dangerously-skip-permissions      │    │
│  │          --bare --no-session-persistence        │    │
│  │          --output-format stream-json            │    │
│  └──────────────────────────────────────────────┘    │
│                        │                              │
│                        ▼                              │
│  ┌──────────────────────────────────────────────┐    │
│  │              Agent Bootstrap Script            │    │
│  │                                                │    │
│  │  1. Authenticate with Taiga + Gitea            │    │
│  │  2. Read ticket + implementation plan          │    │
│  │  3. Clone repo, create branch                  │    │
│  │  4. Determine required tools, install them     │    │
│  │  5. Build system prompt with context            │    │
│  │  6. Invoke Claude Code with task                │    │
│  │  7. On completion: create PR, update ticket     │    │
│  │  8. Report status back to orchestrator          │    │
│  └──────────────────────────────────────────────┘    │
│                                                       │
│  ENV: TICKET_ID, AGENT_ID, AGENT_SPECIALIZATION,     │
│       GITEA_URL, TAIGA_URL, AGENT_TOKEN,              │
│       PLAN_STEP                                       │
└─────────────────────────────────────────────────────┘
```

#### Agent Identity and Role Delegation

Taiga does not support assigning tickets to roles, but it does support **tags** on user stories — freeform labels that are filterable via the API (`GET /api/v1/userstories?tags=frontend`). Role-based delegation is implemented using **delegation tags**:

- When a general-purpose agent wants to delegate work to a specialization, it adds a delegation tag to the user story (e.g., `delegate:frontend`, `delegate:test`) and posts a comment with instructions for the specialized agent.
- The orchestrator watches for tag changes via Taiga webhooks. When a `delegate:<specialization>` tag is detected, the orchestrator:
  1. Finds an available agent of that specialization (or creates one on demand)
  2. Assigns the ticket (via `assigned_users`) to the specialized agent user
  3. Removes the `delegate:` tag and adds an `active:<specialization>` tag for tracking
  4. Spawns the agent container
- When the specialized agent finishes, the orchestrator removes the `active:` tag. The general-purpose agent resumes once no `active:` tags remain.

This approach uses Taiga's native features — tags are visible in the UI as colored badges, filterable via the API, and do not require creating fake users. The delegation flow is transparent: the human can see which specializations were requested and which agents are actively working.

Optionally, **Taiga swimlanes** can be configured per specialization (Frontend, Backend, etc.) to provide a visual Kanban board layout grouped by team. Swimlanes are set via the `swimlane` field on user stories and are filterable via `?swimlane={id}`.

**Agent naming convention:** `<specialization>-agent-<number>`, e.g., `frontend-agent-1`, `backend-agent-2`, `general-agent-1`. The orchestrator auto-creates these users in both Gitea and Taiga when scaling up.

#### State Management and Recovery

All orchestrator state is stored in a Kubernetes ConfigMap (`orchestrator-state` in the `agents` namespace). The state includes:

```yaml
agent_registry:
  - id: "general-agent-1"
    specialization: "general"
    status: "busy"           # idle | busy | creating | destroying
    current_ticket: 42
    gitea_user_id: 5
    taiga_user_id: 8
    job_name: "agent-worker-ticket-42"

ticket_queue:               # FIFO queue of ready tickets
  - ticket_id: 55
    queued_at: "2026-03-25T10:00:00Z"

ticket_assignments:
  - ticket_id: 42
    primary_agent: "general-agent-1"
    delegated_to: ["frontend-agent-1"]
    plan_step: 3
    status: "in_progress"

escalation_tracker:
  - ticket_id: 43
    reassignment_count: 1    # escalate at 2
```

**Recovery on restart:**
1. Load state from ConfigMap
2. List all active K8s Jobs in the `agents` namespace
3. Query Taiga for current ticket states and assignments
4. Reconcile: if a Job exists but ConfigMap says "idle", the agent crashed — reassign the ticket. If ConfigMap says "busy" but no Job exists, create a new Job to resume the work.
5. Poll Taiga for any events missed while down (using `modified_date` filter)

#### Context Preservation

Agents are stateless — their containers are ephemeral. Context is preserved through:

1. **Git branches:** All in-progress work is committed and pushed before the agent exits. When a new agent picks up the same ticket, it checks out the existing branch.
2. **Ticket comments:** Agents document their progress, decisions, and intermediate results in ticket comments. A new agent reads the comment history to understand context.
3. **Implementation plan:** The plan document in the repo tracks which steps are complete (via merged PRs) and which are pending.
4. **PR history:** Review comments, requested changes, and discussions are preserved in Gitea.

When an agent needs to respond to PR review feedback:
1. The orchestrator detects new PR comments via Gitea webhooks (or polling)
2. It spawns a new agent Job with environment variables pointing to the specific PR
3. The agent reads the PR diff, review comments, and ticket context
4. It makes the requested changes on the same branch, pushes, and updates the PR

#### Notification System

The orchestrator exposes a local notification endpoint and supports multiple backends:

1. **Primary: Local webhook** — The orchestrator POSTs JSON payloads to a configurable local URL. A small companion web app (included in the deployment) provides a notification dashboard accessible at `http://localhost:<port>/notifications`.
2. **Optional: Desktop notifications** — via `notify-send` (Linux) triggered by the webhook receiver.
3. **Optional: Local email** — via a local SMTP relay (e.g., MailHog deployed in k3s) for users who prefer email notifications.

Notification events:
- Ticket escalated to human (permanent error, unresolvable conflict, agent disagreement)
- PR ready for human review
- Implementation plan ready for approval
- Ticket moved to "ready for test"
- Quota threshold reached

#### Network Policies

```yaml
# Agent workers: read-only internet, full access to local services
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: agent-worker-policy
  namespace: agents
spec:
  podSelector:
    matchLabels:
      role: agent-worker
  policyTypes: [Egress]
  egress:
  - to:                              # Local services
    - namespaceSelector:
        matchLabels:
          name: gitea
    - namespaceSelector:
        matchLabels:
          name: taiga
  - to:                              # Internet (read-only enforced at app level)
    - ipBlock:
        cidr: 0.0.0.0/0
        except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16]
    ports:
    - protocol: TCP
      port: 443
    - protocol: TCP
      port: 80
```

#### Secret Management

Secrets are stored as Kubernetes Secrets in the `agents` namespace:

| Secret | Contents | Used By |
|---|---|---|
| `anthropic-api-key` | (Optional) Anthropic API key — takes precedence over credentials file if present | Agent Jobs, Orchestrator (PR review) |
| `claude-credentials` | (Optional) Claude Code credentials file (`~/.claude/.credentials.json`) — copied from the host during setup; each agent gets its own copy and handles OAuth refresh independently | Agent Jobs, Orchestrator (PR review) |
| `orchestrator-admin` | Gitea admin token, Taiga admin credentials | Orchestrator (user management) |
| `agent-credentials` | Per-agent Gitea tokens and Taiga credentials | Injected into agent Jobs at creation |
| `webhook-secret` | HMAC key for Taiga webhook signature verification | Orchestrator |

The orchestrator creates per-agent credentials when provisioning new agent identities and stores them as entries in the `agent-credentials` Secret.

#### Permission Policies

Tool-usage policies are stored as a ConfigMap (`agent-policies`) and injected into agent containers. The policy file controls what tools Claude Code can use:

```yaml
# agent-policies ConfigMap
general:
  allowed_tools:
    - Read
    - Edit
    - Write
    - Glob
    - Grep
    - Bash
  bash_restrictions:
    - "no rm -rf /"
    - "no curl POST to external URLs"

specializations:
  test:
    additional_tools: []
  documentation:
    allowed_tools:
      - Read
      - Edit
      - Write
      - Glob
      - Grep
```

Only the human can modify this ConfigMap (enforced by Kubernetes RBAC — agent service accounts have no write access to ConfigMaps).

---

## Implementation Steps

### Step 1: Infrastructure Foundation — k3s, Namespaces, Storage

Set up the base k3s cluster and namespace structure.

**Deliverables:**
- k3s installation script (`scripts/install-k3s.sh`)
- Namespace definitions (`k8s/namespaces.yaml`) for `gitea`, `taiga`, `agents`
- Namespace labels for network policy selectors
- StorageClass configuration (local-path-provisioner, default with k3s)
- Base ConfigMaps for service endpoint discovery

**Can be parallelized with:** Nothing — this is the foundation.

---

### Step 2: Gitea Deployment

Deploy Gitea with PostgreSQL on k3s using the official Helm chart.

**Deliverables:**
- Helm values file (`k8s/gitea/values.yaml`) with admin user, Actions enabled, auto-delete branches on merge
- Deployment instructions
- NodePort or Ingress configuration for `localhost:3000`
- Verification script to confirm Gitea is running and API accessible

**Depends on:** Step 1

**Can be parallelized with:** Step 3 (Taiga deployment)

---

### Step 3: Taiga Deployment

Deploy Taiga (back, front, events, async, PostgreSQL, RabbitMQ) on k3s.

**Deliverables:**
- Kubernetes manifests converted from official docker-compose (`k8s/taiga/`)
- Taiga backend, frontend, events, async (Celery), PostgreSQL, RabbitMQ as separate Deployments/Services
- PersistentVolumeClaims for PostgreSQL data
- NodePort or Ingress for frontend access
- Initial project creation script (creates the single Taiga project, configures statuses: "ready", "in progress", "ready for test")
- Verification script

**Depends on:** Step 1

**Can be parallelized with:** Step 2 (Gitea deployment)

---

### Step 4: Orchestrator Scaffold — Project Structure and API Clients

Create the orchestrator Go project with Taiga and Gitea API client libraries.

**Deliverables:**
- Go module structure (`orchestrator/`)
- `go.mod` with dependencies (`k8s.io/client-go`, `chi` or `net/http`, standard library)
- Taiga API client (`orchestrator/pkg/taiga/`) — authentication (JWT), CRUD for user stories, comments, tags, user management, status transitions, webhook configuration
- Gitea API client (`orchestrator/pkg/gitea/`) — authentication, repo management, PR operations, user management, branch operations
- Configuration structs (`orchestrator/pkg/config/`) with YAML/JSON deserialization
- Unit tests for API clients (mocked with `httptest`)
- Dockerfile for the orchestrator (multi-stage build, distroless base image)

**Depends on:** Steps 2, 3 (needs running Gitea/Taiga for integration testing)

**Can be parallelized with:** Step 5 (agent container image)

---

### Step 5: Agent Container Image

Build the Docker image for agent worker containers.

**Deliverables:**
- Dockerfile (`agent/Dockerfile`) based on `node:22-slim` with Claude Code CLI, git, common build tools
- Agent bootstrap script (`agent/bootstrap.sh`) that:
  - Reads environment variables (ticket ID, credentials, plan step)
  - Authenticates with Taiga and Gitea
  - Reads the ticket and implementation plan
  - Clones the repo, checks out or creates the working branch
  - Determines required tools and installs them
  - Builds the system prompt with full context
  - Invokes `claude -p` with appropriate flags
  - On completion: creates/updates PR, updates ticket, exits
- Agent system prompt template (`agent/system-prompt.md`)
- Container registry setup (local registry in k3s or direct image import)
- Unit tests for bootstrap logic

**Depends on:** Step 1

**Can be parallelized with:** Step 4 (orchestrator scaffold)

---

### Step 6: Webhook Listener and Event Processing

Implement the orchestrator's webhook receiver for Taiga events.

**Deliverables:**
- HTTP webhook endpoint (`orchestrator/pkg/webhooks/handler.go`)
- HMAC signature verification for Taiga webhooks (`X-Hub-Signature` header, SHA-1)
- Event router: dispatches to appropriate handlers based on event type and action
- Event handlers for:
  - Ticket status changed to "ready" → enqueue ticket
  - Ticket tags changed (delegation tags added) → handle delegation flow
  - Ticket assignment changed → track agent status
  - New comment on ticket → check if it's new instructions for a paused agent
- Taiga webhook auto-configuration (registers the webhook on the Taiga project at startup)
- Startup polling: queries Taiga for tickets modified since last shutdown
- Integration tests

**Depends on:** Step 4

---

### Step 7: Agent Identity Management

Implement automatic creation and management of agent identities in Gitea and Taiga.

**Deliverables:**
- Identity manager (`orchestrator/pkg/identity/manager.go`)
- Creates agent users on demand (`<specialization>-agent-<n>`) in both Gitea and Taiga
- Stores credentials in Kubernetes Secret `agent-credentials`
- Adds agent users as members of the Taiga project with appropriate roles
- Adds agent users as collaborators on Gitea repos as needed
- Configures Taiga swimlanes per specialization (Frontend, Backend, Test, Documentation, Operations) for visual Kanban grouping
- Deactivates/removes agent users when scaling down
- Integration tests

**Depends on:** Step 4

**Can be parallelized with:** Step 6 (webhook listener)

---

### Step 8: Ticket Assignment Engine

Implement the FIFO ticket assignment with concurrency control and role delegation.

**Deliverables:**
- Assignment engine (`orchestrator/pkg/assignment/engine.go`)
- FIFO queue backed by the state ConfigMap
- Configurable max concurrency
- Tag-based delegation logic:
  - Detects `delegate:<specialization>` tags on user stories (via webhook events)
  - Finds or creates an available agent of that specialization
  - Assigns the ticket to the specialized agent user via `assigned_users`
  - Replaces `delegate:` tag with `active:<specialization>` tag for tracking
  - Supports multiple concurrent delegation tags (parallel specialized work)
- Escalation tracker: counts no-op reassignment cycles, escalates to human at threshold (2)
- General-purpose agent resume logic: detects when all `active:` tags are removed, triggers general-purpose agent to resume
- Unit and integration tests

**Depends on:** Steps 6, 7

---

### Step 9: Agent Lifecycle Manager

Implement creation and destruction of agent worker Kubernetes Jobs.

**Deliverables:**
- Lifecycle manager (`orchestrator/pkg/lifecycle/manager.go`)
- Creates K8s Jobs with:
  - Agent container image
  - Environment variables (ticket ID, credentials, plan step, URLs)
  - Resource limits (configurable per specialization)
  - Active deadline (configurable timeout)
  - TTL after finished (configurable, default 5 minutes for log access)
- Monitors Job status (running, succeeded, failed)
- Handles Job failure: retries up to configurable limit, then escalates
- Idle timeout: destroys containers that have been idle beyond threshold
- Integrates with assignment engine (notified of new assignments)
- Integration tests

**Depends on:** Step 8

---

### Step 10: Implementation Plan Workflow

Implement the workflow for creating and managing implementation plans.

**Deliverables:**
- Plan workflow service (`orchestrator/pkg/plan/workflow.go`)
- When a ticket is first assigned:
  1. Spawns an agent to analyze the ticket and create an implementation plan
  2. Agent creates the plan as a markdown file and opens a PR
  3. Orchestrator detects the plan PR (by branch naming convention or label)
  4. Optionally requests specialized agents to review the plan PR
  5. Waits for human approval
- After plan approval:
  1. Parses the plan to extract steps, their order, and parallelism markers
  2. Begins executing steps sequentially (or in parallel where marked)
  3. For each step: spawns an agent Job with the step context
- Plan revision detection: when an agent creates a plan-update PR, the workflow pauses implementation until the revision is approved
- Integration tests

**Depends on:** Step 9

---

### Step 11: PR Review Integration

Implement the orchestrator's PR review capability and PR lifecycle management.

**Deliverables:**
- PR review service (`orchestrator/pkg/review/service.go`)
- Watches Gitea for new PRs from agent branches (via Gitea webhooks or polling)
- For each new PR:
  1. Invokes Claude Code CLI (`claude -p`) as a subprocess to review the diff
  2. Posts review comments on the Gitea PR
  3. If changes needed: requests changes and updates ticket status
  4. If approved by orchestrator: adds "orchestrator-approved" label (human still must approve)
- PR feedback loop: detects new review comments from humans, spawns agent to address them
- Ensures PRs contain a link to the ticket and the plan step reference
- Integration tests

**Depends on:** Step 10

---

### Step 12: State Management and Recovery

Implement robust state persistence and restart recovery.

**Deliverables:**
- State manager (`orchestrator/pkg/state/manager.go`)
- Persists orchestrator state to ConfigMap on every state change (debounced)
- State schema (Go structs with JSON tags):
  - Agent registry (id, specialization, status, current ticket, job name)
  - Ticket queue (FIFO, with timestamps)
  - Ticket assignments (primary agent, delegated agents, current plan step)
  - Escalation tracker (reassignment counts per ticket)
- Recovery procedure on startup:
  1. Load state from ConfigMap
  2. List active K8s Jobs, reconcile with state
  3. Query Taiga for ticket states, reconcile with state
  4. Re-enqueue any "ready" tickets not in queue
  5. Restart Jobs for in-progress tickets that have no running Job
- State versioning with optimistic locking (ConfigMap resourceVersion)
- Integration tests including simulated crash recovery

**Depends on:** Steps 8, 9

**Can be parallelized with:** Step 11 (PR review)

---

### Step 13: Notification System

Implement the local notification mechanism.

**Deliverables:**
- Notification service (`orchestrator/pkg/notifications/service.go`)
- Local webhook dispatcher: POSTs structured JSON to a configurable URL
- Notification web dashboard:
  - Small Go HTTP handler + embedded HTML/JS (served from orchestrator itself via `embed` package)
  - Displays recent notifications, filterable by type
  - Accessible at `http://localhost:<port>/notifications`
- Notification types: escalation, PR-ready-for-review, plan-ready-for-approval, ticket-ready-for-test, quota-warning
- Desktop notification integration via `notify-send` (optional, Linux)
- Notification preferences ConfigMap (which events to notify about)
- Tests

**Depends on:** Step 6

**Can be parallelized with:** Steps 10, 11, 12

---

### Step 14: CI/CD Setup — Gitea Actions Runners and Default Workflows

Set up Gitea Actions runners and default workflow templates.

**Deliverables:**
- Act runner Deployment in k3s (`k8s/gitea/act-runner.yaml`) using DinD mode
- Runner registration automation (token generation + injection)
- Default workflow templates:
  - `test.yaml` — runs tests on PR and push to main
  - `pre-release.yaml` — builds and tags pre-release on topic branch merge
  - `release.yaml` — builds and publishes release on main tag
- Template injection: orchestrator applies default workflows when creating new repos
- Documentation of how to customize workflows per project
- Integration tests

**Depends on:** Step 2

**Can be parallelized with:** Steps 10-13

---

### Step 15: Configuration, Policies, and Documentation

Finalize configuration system, permission policies, and project documentation.

**Deliverables:**
- Master configuration file (`config.yaml`) with all tunable parameters:
  - Max concurrency
  - Agent timeout and retry limits
  - Idle container timeout
  - Notification preferences
  - Escalation thresholds
- Permission policy ConfigMap (`agent-policies`) with per-specialization tool restrictions
- Kubernetes RBAC: agent service accounts cannot modify ConfigMaps/Secrets
- Comprehensive `README.md`:
  - Quick start guide
  - Architecture overview
  - Configuration reference
  - Troubleshooting guide
- Typical toolset documentation (per language/framework)
- Integration test suite covering the full workflow (ticket → plan → implementation → PR → review → completion)

**Depends on:** All previous steps

---

## Step Dependency and Parallelism Overview

```
Step 1: Infrastructure Foundation
  ├── Step 2: Gitea Deployment ──────────────────── Step 14: CI/CD Setup
  │     │
  ├── Step 3: Taiga Deployment
  │     │
  │     ▼
  │   Steps 2+3 complete
  │     │
  │     ├── Step 4: Orchestrator Scaffold ─┬── Step 6: Webhooks ──── Step 13: Notifications
  │     │                                  │     │
  │     │                                  │     ├── Step 7: Identity Mgmt
  │     │                                  │     │     │
  │     │                                  │     │     ▼
  │     │                                  │     │   Step 8: Assignment Engine
  │     │                                  │     │     │
  │     │                                  │     │     ▼
  │     │                                  │     │   Step 9: Lifecycle Manager
  │     │                                  │     │     │
  │     │                                  │     │     ├── Step 10: Plan Workflow
  │     │                                  │     │     │     │
  │     │                                  │     │     │     ▼
  │     │                                  │     │     │   Step 11: PR Review
  │     │                                  │     │     │
  │     │                                  │     │     └── Step 12: State & Recovery
  │     │                                  │     │
  │     └── Step 5: Agent Container Image ─┘
  │
  └── All steps complete → Step 15: Configuration & Documentation
```

**Parallel tracks:**
- Steps 2 + 3 run in parallel (Gitea + Taiga)
- Steps 4 + 5 run in parallel (Orchestrator scaffold + Agent image)
- Steps 6 + 7 run in parallel (Webhooks + Identity, once Step 4 is done)
- Steps 11 + 12 + 13 run in parallel (PR review + State + Notifications)
- Step 14 runs independently after Step 2

---

## Key Technical Decisions

| Decision | Choice | Rationale |
|---|---|---|
| **Orchestrator language** | Go | Lower memory footprint, faster startup; `client-go` is the canonical K8s client; goroutines for concurrent agent management; compiles to single static binary (~20 MB container image); `claude -p` subprocess invocation is straightforward via `os/exec` |
| **Orchestrator type** | Traditional service (not AI) | Deterministic control logic (FIFO, concurrency, lifecycle) is better as conventional code; avoids token cost for routine operations |
| **Container runtime** | k3s (Kubernetes) | Lightweight, single-binary; provides Jobs, DNS, Secrets, RBAC; eases future scaling |
| **Agent execution** | K8s Jobs with Claude Code CLI | Clean lifecycle (create→run→complete→cleanup); built-in retry, timeout, status tracking |
| **Agent invocation** | CLI (`claude -p`) wrapped in bootstrap script | Simpler than SDK in container context; stream-json output for monitoring |
| **Role delegation** | Delegation tags on Taiga user stories | Native Taiga feature; `delegate:<specialization>` tags are API-filterable, visible as colored badges in UI, and don't require creating fake placeholder users |
| **State persistence** | Kubernetes ConfigMap | Native to k3s, survives Pod restarts; no additional database needed; fits the state size |
| **Ticket type** | Taiga User Stories | Only type supporting multi-assignment (`assigned_users`); Tasks/Issues only support single assignment |
| **Inter-agent communication** | Taiga comments + tags + assignment changes | Transparent to human; no hidden channels; orchestrator monitors via webhooks |
| **PR linking** | Ticket link + plan step reference in PR description | Minimal overhead; human can navigate from PR → ticket → plan |
| **Notifications** | Local web dashboard + optional desktop notifications | Works fully locally; no external service dependency |
| **CI/CD runners** | Gitea Actions act_runner in DinD mode, separate from agent containers | Isolated from agent work; standard GitHub Actions compatibility |

---

## Phase 2: Ticket Lifecycle Implementation

Steps 1-15 above describe the infrastructure and orchestrator scaffold that is now in
place.  Phase 2 implements the full ticket lifecycle as defined in the [mermaid flow
diagram](mermaid.md): analysis → plan → step-by-step implementation → review → completion.

All architectural decisions are documented with rationale in `mermaid.md` (Gap Analysis
section).

### Key Architectural Decisions (Phase 2)

| Decision | Choice | Rationale |
|---|---|---|
| **Step management** | Agent-driven (not orchestrator) | The agent owns the plan and step progression.  The orchestrator spawns/re-spawns agents but never parses or tracks the plan.  Simpler orchestrator; agent has full context. |
| **Parallel steps** | Deferred (sequential only) | Avoids merge conflicts and complex coordination.  Can be added later. |
| **Analysis Job weight** | Full-weight (clones repo) | Agent needs repo access for proper evaluation.  Whether analysis and plan creation merge into one Job is an implementational decision — clone overhead is negligible. |
| **Ticket status during analysis** | Stays "ready" | Moves to "in-progress" only after the agent confirms it can proceed.  Assignment engine tracks the ticket internally to prevent double-assignment. |
| **Re-spawn trigger** | Ticket unassigned (not any human activity) | Allows human to add info in multiple comments before signaling "ready" by unassigning. |
| **PR event source** | Gitea webhooks on same port (8080) | Listens to `pull_request` and `pull_request_review` events.  Auto-review triggered by `pull_request:opened`. |
| **Auto-review scope** | All PRs on watched repos | Including human-opened PRs.  Post-review lifecycle events silently ignored for PRs with no ticket mapping. |
| **Auto-review type** | COMMENT only | Human decides on approve/reject.  Auto-review comments are processed together with human feedback. |
| **Agent modes** | Separate system prompts per mode | One focused template file per mode: analysis, plan, step, PR-fix.  Bootstrap.sh selects mode via `MODE` env var. |
| **Branch naming** | `ticket-{id}/work`, `ticket-{id}/plan`, `ticket-{id}/step-{n}` | No agent-id in branch names.  Base branch configurable via `base:` field in ticket (default: `main`). |
| **PR-to-ticket mapping** | Both: orchestrator state + PR body parsing | Populated from agent completion comment; falls back to parsing ticket ID from PR body/title. |
| **Step progress signal** | Taiga comment with `[step:N/M]` or `[step:complete]` | Orchestrator reads agent completion comment to decide re-spawn vs. transition to "ready for test". |
| **PR rejection** | Escalate | Post Taiga comment, assign to human, pause.  Human decides whether to keep/delete work branch before re-queuing. |
| **Re-engagement** | Full restart on existing work | Human moves ticket back to "ready".  Agent reuses existing work branch and updates plan. |
| **`pkg/plan/` code** | Remove | Agent manages steps; orchestrator doesn't need plan parsing.  Reuse individual functions elsewhere if applicable. |

---

### Step 16: Foundation Rework and Cleanup

Rework existing orchestrator and agent code to align with the Phase 2 architecture before
adding new features.

**Deliverables:**

- **Remove `pkg/plan/`** — delete `orchestrator/pkg/plan/` package.  Remove references from
  `pkg/state/manager.go` (`Plans` field in state), `cmd/orchestrator/main.go`, and any
  imports.  Clean up tests.
- **Assignment flow** — rework `assignTicket()` in `main.go` to NOT set ticket status to
  "in-progress" at assignment time.  The ticket stays "ready"; only the internal assignment
  engine tracks it.  Status transitions to "in-progress" when the analysis agent confirms
  "proceed" (implemented in Step 19).
- **Human input trigger** — rework `isHumanInput()` in `main.go` to trigger ONLY on
  "ticket unassigned" events (check webhook diff for `assigned_to` changing from a
  non-null value to null), not on any human comment or edit.  Existing comment-based
  detection is removed.
- **Branch naming** — update `bootstrap.sh` branch creation to use
  `ticket-{id}/work` instead of `agent/{agent-id}/ticket-{id}/work`.  No agent-id in
  branch names.
- **`base:` field** — add parsing of optional `base: <branch>` lines in ticket
  descriptions (alongside existing `repo:` parsing in `bootstrap.sh`).  Default to `main`
  when not specified.
- **PR-to-ticket mapping** — add a `PRMappings map[string]int` field to the orchestrator
  state (`pkg/state/manager.go`).  Key: `{owner}/{repo}#{pr_number}`, value: ticket ID.
  Populated when agents report PR creation; used to route Gitea PR events to tickets.
- **`AgentJobSpec` mode field** — add a `Mode string` field to
  `pkg/lifecycle/manager.go:AgentJobSpec`.  Passed as `MODE` env var to the agent
  container.  Valid values: `analysis`, `plan`, `step`, `fix`.

**Tests:** Update all affected unit tests.  Verify that reconciliation loop no longer
re-enqueues tickets that are "ready" but tracked by the assignment engine.

**Depends on:** Nothing (reworks existing code).

---

### Step 17: Gitea Client Extensions

Extend the Gitea API client with methods needed for PR lifecycle management.

**Deliverables:**

- **`EditPullRequest(owner, repo string, number int, opts *EditPRRequest) error`** — update
  PR assignees and reviewers after creation.  Uses `PATCH /repos/{owner}/{repo}/pulls/{index}`.
  `EditPRRequest` struct includes `Assignees []string`.
- **`GetPRDiff(owner, repo string, number int) (string, error)`** — fetch the actual diff
  for a PR.  Uses `GET /repos/{owner}/{repo}/pulls/{index}.diff` (or the `Accept: text/plain`
  header on the PR endpoint).  Replaces the placeholder in `pkg/review/service.go:getPRDiff()`.
- **`GetPRReviews(owner, repo string, number int) ([]PRReview, error)`** — fetch all
  reviews on a PR.  Uses `GET /repos/{owner}/{repo}/pulls/{index}/reviews`.
- **`GetPRReviewComments(owner, repo string, number int, reviewID int64) ([]ReviewComment, error)`**
  — fetch review comments with file paths and line numbers.  Uses
  `GET /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments`.
- **`CreateOrgWebhook(org string, hook *CreateHookRequest) error`** or
  **`CreateRepoWebhook(owner, repo string, hook *CreateHookRequest) error`** — register
  Gitea webhooks.  Used by the orchestrator to set up PR event delivery.
- **Structs:** `EditPRRequest`, `PRReview`, `ReviewComment` (with `Path`, `Line`,
  `Body` fields), `CreateHookRequest`.
- **Update `PullRequest` struct** — add `User` (author), `Assignees`, `MergedBy`,
  `Merged` boolean fields.

**Tests:** Unit tests with `httptest` mocks for all new methods.

**Depends on:** Nothing (extends existing client).

**Can be parallelized with:** Step 16.

---

### Step 18: Gitea Webhook Handler

Add a Gitea webhook receiver to the orchestrator so it can react to PR events.

**Deliverables:**

- **Gitea webhook handler** (`orchestrator/pkg/webhooks/gitea_handler.go`) — parses Gitea
  webhook payloads for `pull_request` and `pull_request_review` events.  Structs for Gitea
  webhook event format (different from Taiga's format).
- **HTTP route** — register `/webhooks/gitea` on the existing HTTP server (port 8080)
  alongside the existing `/webhooks/taiga` route.  Optionally verify Gitea webhook secret
  (HMAC-SHA256).
- **Event routing in `main.go`** — handle:
  - `pull_request` action `opened` → trigger auto-review (Step 21).
  - `pull_request` action `closed` + `merged: true` → re-spawn agent for next step (Step 23).
  - `pull_request` action `closed` + `merged: false` → PR rejected, escalate (Step 24).
  - `pull_request_review` action `submitted` + review type `request_changes` (from human) →
    spawn PR-fix agent (Step 22).
- **PR-to-ticket resolution** — on each PR event, look up ticket ID from
  `state.PRMappings`.  If not found, parse ticket ID from PR body/title.  If still not
  found (human-opened PR with no ticket reference), proceed only for auto-review; silently
  ignore lifecycle events.
- **Webhook registration** — on first encounter of a repo (when an agent creates a PR), the
  orchestrator registers a webhook on that repo via the Gitea API (Step 17).  Stores
  registered repos in state to avoid duplicate registrations.  Alternatively, register a
  system-level or org-level webhook in `init-gitea.sh`.

**Tests:** Unit tests for event parsing and routing.  Integration test with a mock Gitea
webhook payload.

**Depends on:** Step 17 (Gitea client webhook registration method).

---

### Step 19: Agent Mode — Analysis

Implement the analysis phase: a dedicated agent Job that evaluates the ticket and decides
whether to proceed or request human input.

**Deliverables:**

- **System prompt** (`agent/system-prompt-analysis.md`) — instructs Claude to:
  - Read the ticket subject, description, and all comments.
  - If a repo is referenced (`repo:` lines), examine the codebase structure.
  - If a `base:` branch is specified, verify it exists.
  - Decide: is the ticket sufficiently clear to create an implementation plan?
  - If yes: post a comment with `[analysis:proceed]` and a brief summary of understanding.
  - If no: post a comment with `[analysis:need-info]` explaining what info is missing,
    then assign the ticket to the human user.
- **Bootstrap mode `analysis`** — in `bootstrap.sh` (or a mode-selected code path):
  - Fetch ticket from Taiga (subject, description, comments).
  - Parse `repo:` and `base:` lines.
  - If a repo is referenced: clone it, provide it as context to Claude.
  - If `base:` specified: check branch exists via `git ls-remote` or Gitea API.
  - Invoke Claude with the analysis system prompt and ticket context.
  - On `[analysis:need-info]`: assign ticket to human (`HUMAN_TAIGA_ID`), exit 0.
  - On `[analysis:proceed]`: exit 0.
- **Orchestrator integration** — after the analysis Job completes:
  - Parse the latest Taiga comment for `[analysis:proceed]` or `[analysis:need-info]`.
  - On `proceed`: set ticket status to "in-progress", spawn plan agent (Step 20).
  - On `need-info`: ticket is now assigned to human, stays "ready".  Orchestrator
    clears internal assignment.  When human unassigns → ticket re-enters the queue →
    full re-analysis.
- **Assignment flow change** — when a "ready" unassigned ticket is detected:
  1. Enqueue and assign internally (assignment engine).
  2. Spawn analysis Job (Mode=`analysis`).
  3. Do NOT change Taiga ticket status.

**Tests:** Unit tests for comment parsing.  Test bootstrap analysis mode with mock Taiga/Gitea.

**Depends on:** Step 16 (assignment rework, `base:` parsing, `MODE` env var).

---

### Step 20: Agent Mode — Plan Creation

Implement the plan creation phase: the agent clones the repo, writes an implementation
plan, and opens a PR.

**Deliverables:**

- **System prompt** (`agent/system-prompt-plan.md`) — instructs Claude to:
  - Read the ticket, all comments, and the full codebase.
  - Create a `ticket-{id}/work` branch from the base branch (default `main`, or `base:`
    value).
  - Create a `ticket-{id}/plan` sub-branch from the work branch.
  - Write `IMPLEMENTATION_PLAN.md` with `### Step N: Title` format (as expected by the
    plan parser convention).
  - Commit the plan and create a PR targeting the work branch.
  - Post a Taiga comment with the PR link and `[phase:plan-created]`.
- **Bootstrap mode `plan`** — in `bootstrap.sh`:
  - Fetch ticket, clone repo, set up git identity.
  - Create or reuse work branch from base branch.
  - Create plan sub-branch from work branch.
  - Invoke Claude with the plan system prompt.
  - Push branch and create PR (assign to the reviewing agent/claude per Step 21 flow).
  - Post completion comment on Taiga ticket.
- **Orchestrator integration** — after analysis succeeds (`[analysis:proceed]`):
  - Spawn plan agent (Mode=`plan`) for the same ticket.
  - The resulting PR triggers auto-review via Gitea webhook (Step 21).

**Tests:** Test bootstrap plan mode.  Verify branch naming convention.

**Depends on:** Step 16 (branch naming), Step 18 (Gitea webhook for PR detection — the
plan PR triggers the auto-review pipeline).

---

### Step 21: Auto-Review Pipeline

Wire the existing `pkg/review/service.go` into the Gitea webhook flow so every new PR
gets an automated Claude review before human review.

**Deliverables:**

- **Fix `getPRDiff()`** — replace the placeholder string in `pkg/review/service.go` with a
  call to `giteaClient.GetPRDiff()` (from Step 17).
- **Auto-review trigger** — in the Gitea webhook handler (Step 18), on
  `pull_request:opened`:
  1. Assign the PR to the reviewing agent/claude identity (via `EditPullRequest`).
  2. Invoke `ReviewPR()` — Claude reviews the diff, posts a `COMMENT` review on Gitea.
  3. After the review posts, reassign the PR to the human user.
- **Non-blocking execution** — the auto-review is a Claude CLI subprocess invocation that
  takes time.  Run it in a goroutine so the webhook handler returns immediately.  Track
  in-progress reviews in orchestrator state to avoid duplicate reviews on rapid webhook
  re-delivery.
- **All PRs** — auto-review fires for all PRs on watched repos (including human-opened PRs).
  For PRs with no ticket mapping, auto-review still runs but no lifecycle actions follow.

**Tests:** Integration test with mock PR diff and Claude review output.

**Depends on:** Step 17 (GetPRDiff, EditPullRequest), Step 18 (Gitea webhook handler).

---

### Step 22: Agent Mode — PR Review-Fix

Implement the PR-fix phase: when a human requests changes on a PR, the orchestrator
spawns the same agent to address the feedback.

**Deliverables:**

- **System prompt** (`agent/system-prompt-fix.md`) — instructs Claude to:
  - Read the full PR diff and all review comments (with file paths and line numbers).
  - Read the ticket context and implementation plan for broader understanding.
  - Address each review comment.
  - Commit changes on the existing branch, push.
  - Post a comment on the PR: "Changes addressed, ready for re-review."
  - Reassign the PR to the human user.
  - Post a Taiga comment with `[fix:applied]` and a brief summary.
- **Bootstrap mode `fix`** — in `bootstrap.sh`:
  - Receive `PR_NUMBER` and `PR_REPO` environment variables (set by orchestrator).
  - Fetch ticket from Taiga, clone repo, check out the existing PR branch.
  - Fetch PR diff and review comments from Gitea API (with file/line references).
  - Format review comments for Claude (file path, line number, comment body).
  - Invoke Claude with the fix system prompt and review context.
  - Push changes, post PR comment, reassign PR, post Taiga comment.
- **Orchestrator trigger** — on `pull_request_review` webhook with action `submitted` and
  type `request_changes`:
  - Skip if reviewer is an agent (only react to human reviews).
  - Look up the ticket via PR-to-ticket mapping.
  - Look up the same agent identity that created the PR (from assignment state).
  - Spawn a PR-fix Job (Mode=`fix`) with `PR_NUMBER` and `PR_REPO` env vars.
- **AgentJobSpec extension** — add `PRNumber int` and `PRRepo string` fields, passed as
  environment variables to the agent container.

**Tests:** Test bootstrap fix mode with mock review comments.

**Depends on:** Step 17 (GetPRReviews, GetPRReviewComments, EditPullRequest),
Step 18 (Gitea webhook handler), Step 21 (auto-review — ensures review flow works).

---

### Step 23: Agent Mode — Step Implementation

Implement the step implementation phase: agent reads the plan, determines the next step,
implements it, and opens a PR.

**Deliverables:**

- **System prompt** (`agent/system-prompt-step.md`) — instructs Claude to:
  - Read the ticket, all Taiga comments, and the implementation plan from the repo.
  - Determine which step to work on next (based on merged PRs and plan state).
  - Create a `ticket-{id}/step-{n}` branch from the work branch.
  - Implement the step.  Run tests.
  - Commit, push, create PR targeting the work branch.
  - Post a Taiga comment: `[step:N/M]` (e.g. `[step:2/5]`) if more steps remain, or
    `[step:complete]` if this was the last step.
  - If the plan needs updating (discovered during implementation), the agent may open a
    plan-update PR instead of a step PR and signal `[plan-update]` in the Taiga comment.
- **Bootstrap mode `step`** — in `bootstrap.sh`:
  - Fetch ticket, clone repo (check out the work branch).
  - Read the implementation plan from the repo.
  - Invoke Claude with the step system prompt and full context.
  - Push branch, create PR, post completion comment on Taiga.
- **Orchestrator trigger** — on `pull_request` webhook with action `closed` + `merged: true`:
  - Look up the ticket via PR-to-ticket mapping.
  - Read the latest Taiga comment from the agent:
    - `[step:N/M]` → spawn step agent again (Mode=`step`).  The new agent reads the
      repo state and picks up the next step.
    - `[step:complete]` → proceed to completion (Step 24).
    - `[plan-update]` → treat like any other PR; after merge, re-spawn agent.
  - If no matching marker found, re-spawn agent anyway (agent decides what to do — per the
    "orchestrator always re-spawns, agent decides" principle).

**Tests:** Test bootstrap step mode.  Test orchestrator comment parsing for step markers.

**Depends on:** Step 20 (plan exists in repo), Step 18 (Gitea webhook for merge detection).

---

### Step 24: Completion Lifecycle

Implement the end-of-lifecycle transitions: ready-for-test, PR rejection escalation, and
re-engagement.

**Deliverables:**

- **Ready for test** — when the orchestrator reads `[step:complete]` from the agent's Taiga
  comment:
  1. Set ticket status to "ready for test".
  2. Assign ticket to the human user.
  3. Post a release notes summary as a Taiga comment (generated by the agent in the
     `[step:complete]` comment or in a separate final comment).
  4. Clear the internal assignment in the assignment engine.
- **PR rejection** — when the Gitea webhook fires `pull_request` with action `closed` +
  `merged: false` (PR closed without merge):
  1. Look up ticket via PR-to-ticket mapping.
  2. Post a Taiga comment: "PR #{number} was rejected by the reviewer.  Ticket paused —
     awaiting human guidance."
  3. Assign ticket to human user.
  4. Clear internal assignment.
  5. Ticket status stays "in-progress".  Human can: add guidance and move to "ready"
     (full restart), or close the ticket.
- **Re-engagement** — when a "ready for test" ticket is moved back to "ready" by the human:
  - The orchestrator's existing reconciliation loop picks it up as an unassigned "ready"
    ticket.
  - Spawns a new analysis Job.  The agent sees the full comment history (including test
    feedback), reuses the existing work branch, and updates the plan as needed.
- **Release notes** — the step agent's `[step:complete]` comment should include a
  human-readable summary of all changes made across the ticket.  The system prompt for
  step mode instructs Claude to write this summary when it determines all steps are done.

**Tests:** Integration test for the full lifecycle: analysis → plan → step → complete.
Test PR rejection escalation.  Test re-engagement flow.

**Depends on:** Step 23 (step signaling), Step 18 (Gitea webhook for PR close events).

---

### Step 25: Documentation Update

Update all documentation to reflect the Phase 2 architecture.

**Deliverables:**

- **README.md** — update the "Usage" section:
  - Document the `base:` field in ticket descriptions (alongside `repo:`).
  - Update the "Ticket Lifecycle" section to match the new flow (analysis → plan → steps).
  - Update the "What Happens After You Create a Ticket" section.
  - Update the "Reviewing and Handling PRs" section (auto-review, PR-fix flow).
  - Remove references to orchestrator-driven plan parsing.
  - Add "Agent Modes" section describing the four modes.
- **README.md** — update the "Architecture" section:
  - Update the Architecture diagram (Gitea webhook, agent modes).
  - Update the Orchestrator description (Gitea webhook handler).
- **README.md** — update the "Agent Worker" section:
  - Document `MODE` environment variable and the four modes.
  - Document `PR_NUMBER`, `PR_REPO` environment variables (for fix mode).
  - Update the agent workflow description.
- **System prompts** — ensure all four system prompt files are complete and consistent:
  - `agent/system-prompt-analysis.md`
  - `agent/system-prompt-plan.md`
  - `agent/system-prompt-step.md`
  - `agent/system-prompt-fix.md`
- **Config documentation** — document any new configuration fields.

**Depends on:** All previous steps.

---

### Phase 2 — Step Dependency and Parallelism Overview

```
Step 16: Foundation Rework ──────────┬── Step 19: Analysis Mode
                                     │
Step 17: Gitea Client Extensions ────┼── Step 18: Gitea Webhook Handler
                                     │     │
                                     │     ├── Step 20: Plan Mode
                                     │     │
                                     │     ├── Step 21: Auto-Review Pipeline
                                     │     │     │
                                     │     │     └── Step 22: PR Review-Fix Mode
                                     │     │
                                     │     └── Step 23: Step Implementation Mode
                                     │           │
                                     │           └── Step 24: Completion Lifecycle
                                     │
                                     └── All steps complete → Step 25: Documentation
```

**Parallel tracks:**
- Steps 16 + 17 run in parallel (independent rework + new client methods)
- Step 18 depends on Step 17; Step 19 depends on Step 16
- Steps 20, 21 depend on Step 18 (both need Gitea webhooks)
- Step 22 depends on Steps 17, 18, 21
- Step 23 depends on Steps 18, 20
- Step 24 depends on Steps 18, 23
- Step 25 depends on all
