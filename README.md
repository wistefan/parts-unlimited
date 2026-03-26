# dev-env

Environment and orchestration setup for software development by Claude AI.

A locally-hosted, Kubernetes-based platform that orchestrates Claude AI agents to autonomously perform software development work driven by tickets from a Taiga project management instance. All components run on a single machine using k3s.

## Prerequisites

- Linux (tested on Ubuntu/Debian-based systems)
- `curl`
- `helm` (v3.x)
- `python3`
- Ports **3000** (Gitea) and **9000** (Taiga) must be free
- Root/sudo access (for k3s installation)
- At least 4 CPU cores and 8 GB RAM recommended

## Quick Start

```bash
# Clone the repo
git clone <repo-url> && cd dev-env

# Bring up the entire environment
sudo ./scripts/setup.sh

# Verify everything is running
./scripts/verify.sh
```

After setup completes:

| Service | URL | Credentials |
|---|---|---|
| Gitea | http://localhost:3000 | `claude` / `password` (system admin) |
| Taiga | http://localhost:9000 | `admin` / `password` (system admin) |

Both services also have a configurable **human user** (default: `wistefan` / `password`) with full admin privileges. See [Configuration](#configuration) for how to customize.

## Usage

This section explains the day-to-day workflow: how to create work for the agents, what to expect while they work, and how to review and merge the results.

### Providing the Anthropic API Key

Before agents can do any work, you must provide your Anthropic API key as a Kubernetes Secret. The orchestrator and all agent workers read it from this secret. You can create or manage API keys at https://console.anthropic.com under **API Keys**.

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

kubectl create secret generic anthropic-api-key \
  --namespace=agents \
  --from-literal=api-key='sk-ant-...'
```

This only needs to be done once. The secret persists across restarts. To rotate the key, delete and recreate the secret:

```bash
kubectl delete secret anthropic-api-key -n agents
kubectl create secret generic anthropic-api-key \
  --namespace=agents \
  --from-literal=api-key='sk-ant-NEW-KEY'
```

### Creating a Ticket

All work starts as a **user story** on the Taiga Kanban board.

1. Open Taiga at http://localhost:9000 and sign in as your human user.
2. Open the **Dev Environment** project.
3. Click **+ Add new** to create a user story.
4. Fill in the fields:

| Field | Required | Description |
|---|---|---|
| **Subject** | Yes | Short title describing the work (e.g., "Add pagination to user list API") |
| **Description** | Yes | Detailed requirements. The more context you provide, the better the agent's output. |
| **Target repository** | No | One or more `repo:` lines in the description (see below). If omitted, the agent asks via a comment. |

5. Set the status to **ready** — this is the signal that tells the orchestrator the ticket is available for an agent to pick up.

#### Specifying repositories

Use `repo:` lines in the ticket description to tell the agent which repositories are involved. Several formats are supported:

```
repo: claude/user-service              # existing local Gitea repo
repo: https://github.com/org/project   # remote repo — agent creates a local copy in Gitea
repo: claude/new-service               # does not exist yet — agent creates it
```

When a remote URL is provided, the agent creates the repository in local Gitea and imports the remote as the initial content. This is useful for contributing to existing projects that are not yet mirrored locally.

Multiple repositories can be listed when a ticket spans several codebases. The agent determines which one is the "main" repo (where most work happens) and creates the implementation plan there. Steps in secondary repos are referenced in the plan, and every PR links back to the ticket.

```
repo: claude/backend-api
repo: claude/frontend-app
```

If no `repo:` line is present, the agent posts a comment on the ticket asking for the target repository before starting work.

#### Example tickets

Simple single-repo ticket:

```markdown
Add a REST endpoint for listing users with cursor-based pagination.

repo: claude/user-service

Requirements:
- GET /api/v1/users with query params `limit` (default 20, max 100) and `cursor`
- Return a JSON response with `items` array and `next_cursor` field
- Include integration tests
```

Multi-repo ticket with a remote source:

```markdown
Add OpenAPI documentation generation to the project and publish it
on the docs site.

repo: https://github.com/acme/billing-api
repo: claude/docs-site

Requirements:
- Generate OpenAPI spec from the existing endpoint annotations in billing-api
- Add a CI step that publishes the spec to the docs-site repo
- Include a rendered Swagger UI page
```

### Ticket Lifecycle

Tickets move through three statuses that agents act on:

```
ready ──→ in progress ──→ ready for test
  │            │                │
  │            │                └─ Agent finished. Human reviews and merges final PR.
  │            └─ Agent is actively working. PRs appear in Gitea.
  └─ Ticket is queued. Next available agent picks it up (FIFO).
```

All other statuses (e.g., "done", "closed") are yours to manage — agents ignore them.

### What Happens After You Create a Ticket

1. **Queue** — The orchestrator detects the new "ready" ticket (via webhook) and adds it to a FIFO queue.
2. **Assignment** — When an agent slot is available (up to `maxConcurrency`, default 3), the orchestrator assigns the ticket, creates an agent identity if needed, and spawns a container.
3. **Implementation plan** — The agent's first action is always to create an **implementation plan** as a markdown document and open a PR for it. The ticket moves to "in progress".
4. **Wait for plan approval** — No implementation begins until you approve the plan PR. Review it, leave comments, request changes — the agent will respond.
5. **Step-by-step implementation** — After plan approval, the agent works through each step, opening one PR per step. Parallelizable steps may be worked on by multiple agents simultaneously.
6. **Final PR** — When the agent opens the last PR, the ticket moves to **ready for test** and the agent posts release notes as a comment on the ticket.

### Reviewing and Handling PRs

Agent PRs appear in Gitea at http://localhost:3000. Each PR includes:
- A link to the Taiga ticket
- A reference to the implementation plan step it addresses
- Test results from the agent's own test run

The review workflow:

1. **Orchestrator review** — The orchestrator automatically runs a Claude-powered code review on every agent PR and posts comments. It cannot approve or merge.
2. **Human review** — Open the PR in Gitea, read the diff and review comments. You have three options:
   - **Approve and merge** — The agent continues to the next step.
   - **Request changes** — Add review comments explaining what to fix. The orchestrator detects the new comments and spawns an agent to address them on the same branch.
   - **Reject** — Close the PR without merging. The agent posts a comment on the ticket and pauses. Add new instructions on the ticket to resume.

Only the human user can approve and merge PRs. Agents and the orchestrator cannot.

### Specialized Work and Delegation

A general-purpose agent may delegate subtasks to specialized agents (Frontend, Backend, Test, Documentation, Operations). Delegation is visible on the ticket as **tags**:

| Tag pattern | Meaning |
|---|---|
| `delegate:frontend` | General agent requested frontend work — waiting for a specialist |
| `active:frontend` | A frontend specialist is actively working |
| (tag removed) | Specialist finished — general agent resumes |

Multiple specializations can run in parallel (e.g., `active:frontend` and `active:test` at the same time). The general-purpose agent resumes once all `active:` tags are gone.

You do not need to manage these tags — the orchestrator handles them automatically. They are visible on the Kanban board as colored badges for transparency.

### Monitoring Progress

- **Taiga ticket comments** — Agents post progress updates, questions, and results as comments on the ticket. This is the primary communication channel.
- **Gitea PRs** — Each PR shows the code changes and review discussion for one implementation step.
- **Notification dashboard** — If configured, visit `http://localhost:8080/notifications` for a feed of events (escalations, PRs ready for review, plan approvals needed).
- **Desktop notifications** — Enable `desktopNotify: true` in `config.yaml` for Linux desktop alerts via `notify-send`.

### Handling Escalations

Agents escalate to the human via ticket comments in these situations:

| Situation | What the agent does | What you should do |
|---|---|---|
| **Unclear requirements** | Posts a comment asking for clarification | Reply on the ticket with the missing information |
| **Merge conflict** | Posts a comment explaining the conflict | Either resolve it yourself or provide guidance in a comment |
| **Repeated delegation failures** | After 2 no-op reassignment cycles, escalates | Check the ticket comments to understand why delegation failed, then provide instructions |
| **Job failure after retries** | Agent failed twice, ticket is flagged | Check the agent logs (`kubectl logs -n agents <pod>`) and the ticket comments, then provide guidance |
| **PR fully rejected** | Agent pauses and comments on the ticket | Add new instructions on the ticket to resume work |

### Working with Multiple Repositories

A single ticket can span multiple repositories (see [Specifying repositories](#specifying-repositories)). The agent decides which repo is the "main" one — where most of the work happens — and creates the implementation plan there. Steps that affect secondary repos are outlined in the plan and result in separate PRs in those repos. Every PR (regardless of repo) links back to the ticket and references its plan step. If merge ordering between repos matters, the agent documents this in the PR descriptions or creates the PRs sequentially.

### Tips for Writing Good Tickets

- **Be specific about acceptance criteria** — agents follow instructions literally.
- **Include the target repo** — add `repo:` lines so the agent can start immediately instead of asking.
- **Reference existing code** — if the change relates to existing files or patterns, mention them (e.g., "follow the same pattern as `pkg/auth/handler.go`").
- **One concern per ticket** — keep tickets focused. The agent creates an implementation plan regardless, but simpler tickets produce better results.
- **Use comments for follow-up** — if you need to add context after creation, post a comment rather than editing the description (agents are notified of new comments via webhooks).

## Scripts

All scripts are in the `scripts/` directory.

### `setup.sh`

Master setup script. Installs k3s, deploys Gitea and Taiga, and initializes both services. Must be run as root.

```bash
sudo ./scripts/setup.sh
```

The script is idempotent — running it again will upgrade existing deployments rather than fail.

**Environment variables for customization:**

| Variable | Default | Description |
|---|---|---|
| `GITEA_PORT` | `3000` | Port for Gitea web UI and API |
| `TAIGA_PORT` | `9000` | Port for Taiga web UI and API |
| `TAIGA_SECRET_KEY` | auto-generated | Django secret key for Taiga |
| `HUMAN_USERNAME` | `wistefan` | Human user created in both Gitea and Taiga |
| `HUMAN_PASSWORD` | `password` | Password for the human user |
| `HUMAN_EMAIL` | `<username>@dev-env.local` | Email for the human user |

The human user is created with **admin privileges** in both Gitea (site admin) and Taiga (superuser), allowing full control over all projects, users, and settings.

Example with custom human user and ports:

```bash
HUMAN_USERNAME=johndoe HUMAN_PASSWORD=secret GITEA_PORT=3001 sudo -E ./scripts/setup.sh
```

### `teardown.sh`

Removes all Kubernetes resources (Helm releases, manifests, PVCs, namespaces).

```bash
# Remove services but keep k3s installed
sudo ./scripts/teardown.sh

# Full cleanup: also uninstall k3s and delete all data
sudo ./scripts/teardown.sh --full
```

### `verify.sh`

Checks that all components are running and accessible. Returns exit code 0 if all checks pass.

```bash
./scripts/verify.sh
```

### `install-k3s.sh`

Standalone k3s installation. Called automatically by `setup.sh` but can be run independently.

```bash
sudo ./scripts/install-k3s.sh
```

### `init-gitea.sh`

Initializes Gitea after deployment: verifies the admin user and creates the `wistefan` user. Called automatically by `setup.sh`.

```bash
GITEA_URL=http://localhost:3000 ./scripts/init-gitea.sh
```

### `init-taiga.sh`

Initializes Taiga after deployment: creates the superuser, project, user story statuses, swimlanes, and the `wistefan` user. Called automatically by `setup.sh`.

```bash
TAIGA_URL=http://localhost:9000 ./scripts/init-taiga.sh
```

**Environment variables:**

| Variable | Default | Description |
|---|---|---|
| `TAIGA_URL` | `http://localhost:9000` | Taiga base URL |
| `TAIGA_ADMIN_USERNAME` | `admin` | Superuser username |
| `TAIGA_ADMIN_PASSWORD` | `password` | Superuser password |
| `TAIGA_PROJECT_NAME` | `Dev Environment` | Project name |

### `wait-for-ready.sh`

Helper that blocks until all pods in a namespace are running and ready.

```bash
./scripts/wait-for-ready.sh <namespace> [timeout_seconds]
# Example: ./scripts/wait-for-ready.sh taiga 300
```

## Architecture

```
k3s (single node)
├── Namespace: gitea
│   ├── Gitea (Helm chart) + PostgreSQL
│   └── (future: act_runner for CI/CD)
├── Namespace: taiga
│   ├── taiga-back (Django API)
│   ├── taiga-front (Angular SPA)
│   ├── taiga-events (WebSocket server)
│   ├── taiga-async (Celery worker)
│   ├── taiga-protected (attachment proxy)
│   ├── taiga-gateway (nginx reverse proxy, port 9000)
│   ├── PostgreSQL 12.3
│   └── RabbitMQ 3.8
├── Namespace: agents (future: orchestrator + worker agents)
└── Storage
    ├── PVCs via local-path-provisioner (PostgreSQL, RabbitMQ)
    └── hostPath volumes at /var/lib/dev-env/taiga/ (shared static/media)
```

### Gitea

Deployed via the [official Helm chart](https://gitea.com/gitea/helm-gitea) with:
- Gitea Actions enabled
- Auto-delete branches on merge
- Webhook allowed host list set to `*` (for receiving webhooks from Taiga/orchestrator)
- Bundled PostgreSQL

Helm values: `k8s/gitea/values.yaml`

### CI/CD — Gitea Actions

A Gitea Actions runner is deployed using Docker-in-Docker mode (`k8s/gitea/act-runner.yaml`). It executes CI/CD workflows in the same k3s environment but in separate containers from the agent workers.

Default workflow templates are in `workflows/`:

| Workflow | Trigger | Purpose |
|---|---|---|
| `test.yaml` | PR and push to main | Auto-detects language (Go/Node/Python), runs tests and linters |
| `pre-release.yaml` | Merge to main | Creates an auto-incremented pre-release tag (`v*-rc.*`) |
| `release.yaml` | Stable version tag (`v*.*.*`) | Builds artifacts and creates a GitHub/Gitea release |

The orchestrator applies these workflows automatically when creating new repositories.

### Taiga

Deployed as individual Kubernetes manifests converted from the [official docker-compose](https://github.com/taigaio/taiga-docker). Changes from the official setup:
- **Single RabbitMQ instance** instead of two (events and async share one instance)
- **hostPath volumes** for shared static/media files (optimized for single-node k3s)
- **Webhooks enabled** with private address access allowed (for in-cluster communication)
- **Public registration enabled** (agents can self-register)

Manifests: `k8s/taiga/`

### Taiga Project Configuration

The init script creates a Taiga project with:

**User story statuses:**
| Status | Purpose |
|---|---|
| ready | Ticket is ready for an agent to pick up |
| in progress | Agent is actively working on the ticket |
| ready for test | Agent has completed work, awaiting human review |

**Swimlanes** (for Kanban board visualization):
- General, Frontend, Backend, Test, Documentation, Operations

## Agent Security Isolation

Agent workers run in sandboxed Kubernetes containers with multiple layers of isolation:

| Layer | Mechanism | Effect |
|---|---|---|
| **Network** | `NetworkPolicy` (`k8s/agents/network-policy.yaml`) | Egress limited to Gitea + Taiga (read/write) and public internet on 80/443 (read-only). All other cluster-internal and LAN traffic denied. No ingress. |
| **Kubernetes API** | `ServiceAccount` with `automountServiceAccountToken: false` | Agent pods cannot read secrets, configmaps, or interact with the K8s API in any way. |
| **Container** | `securityContext` on pod and container | Non-root user (UID 1000), no privilege escalation, all Linux capabilities dropped. |
| **Filesystem** | `emptyDir` workspace volume | Agent works in ephemeral storage. No hostPath mounts, no access to host filesystem. |
| **RBAC** | Separate `orchestrator` ServiceAccount | Only the orchestrator can manage Jobs, read secrets, and update state. |

The `--dangerously-skip-permissions` flag on `claude -p` bypasses Claude Code's own interactive permission prompts (file edits, bash commands) — it does NOT bypass Kubernetes network policies or Linux security contexts. The container sandbox enforces the actual security boundaries.

Manifests: `k8s/agents/`

## Kubernetes Access

After setup, `kubectl` is configured automatically:

```bash
# If kubectl doesn't work, set the kubeconfig explicitly
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# Check cluster status
kubectl get nodes
kubectl get pods -A
```

## Troubleshooting

### Port conflicts

If ports 3000 or 9000 are in use, either stop the conflicting service or use custom ports:

```bash
# Check what's using a port
sudo ss -tlnp | grep ':3000'

# Stop a Docker container on that port
docker stop <container-name>

# Or use different ports
GITEA_PORT=3001 TAIGA_PORT=9001 sudo -E ./scripts/setup.sh
```

### Pods not starting

```bash
# Check pod status
kubectl get pods -n taiga
kubectl get pods -n gitea

# Check logs for a failing pod
kubectl logs -n taiga <pod-name>

# Describe pod for events
kubectl describe pod -n taiga <pod-name>
```

### Taiga backend restarting

The Taiga backend depends on PostgreSQL and RabbitMQ. If they are slow to start, the backend will restart a few times — this is expected. The readiness probes will handle it.

### Re-running setup

The setup script is idempotent. If something fails midway, re-run it:

```bash
sudo ./scripts/setup.sh
```

### Complete reset

```bash
sudo ./scripts/teardown.sh --full
sudo ./scripts/setup.sh
```

## Agent Worker

The agent worker is a Docker container that runs Claude Code CLI to perform autonomous coding tasks. It lives in `agent/` and contains:

- **Dockerfile** — Based on `node:22-slim` with Claude Code CLI, git, and common build tools
- **Bootstrap script** (`bootstrap.sh`) — Orchestrates the full agent lifecycle: authenticates with Taiga/Gitea, fetches the ticket, clones the repo, builds a task prompt with full context, invokes Claude Code, pushes changes, and creates a PR
- **System prompt** (`system-prompt.md`) — Coding guidelines, quality standards, and completion protocol for the agent

### Building

```bash
cd agent
docker build -t agent-worker:latest .
```

### Environment Variables

| Variable | Required | Description |
|---|---|---|
| `TICKET_ID` | Yes | Taiga user story ID to work on |
| `AGENT_ID` | Yes | Agent identity (e.g., `general-agent-1`) |
| `AGENT_SPECIALIZATION` | Yes | Agent role (e.g., `general`, `frontend`) |
| `GITEA_URL` | Yes | Gitea base URL |
| `GITEA_USERNAME` | Yes | Gitea credentials for this agent |
| `GITEA_PASSWORD` | Yes | |
| `TAIGA_URL` | Yes | Taiga base URL |
| `TAIGA_USERNAME` | Yes | Taiga credentials for this agent |
| `TAIGA_PASSWORD` | Yes | |
| `ANTHROPIC_API_KEY` | Yes | Shared Anthropic API key |
| `PLAN_STEP` | No | Implementation plan step number |
| `REPO_OWNER` | No | Gitea repo owner (extracted from ticket if not set) |
| `REPO_NAME` | No | Gitea repo name (extracted from ticket if not set) |
| `ALLOWED_TOOLS` | No | Space-separated Claude tools (default: all) |

### Agent Workflow

1. Validates environment variables
2. Authenticates with Taiga (JWT) and Gitea (basic auth)
3. Fetches the ticket (subject, description, comments)
4. Determines the target repo (from env vars or ticket description, format: `repo: owner/name`)
5. Clones the repo and creates/resumes a branch (`agent/<id>/ticket-<id>/step-<n>`)
6. Reads the implementation plan if present
7. Builds a task prompt with full ticket context
8. Invokes `claude -p --dangerously-skip-permissions` with the task prompt
9. Pushes changes and creates a PR with a link to the ticket
10. Posts a progress comment on the ticket

### Testing

```bash
bash agent/test-bootstrap.sh
```

## Orchestrator

The orchestrator is a Go service that coordinates agent workers. It lives in `orchestrator/` and provides:

- **Taiga API client** (`pkg/taiga/`) — JWT authentication, user stories CRUD, comments, tags, statuses, webhooks, memberships, roles
- **Gitea API client** (`pkg/gitea/`) — basic auth, repos, pull requests, reviews, comments, users, branches
- **Webhook handler** (`pkg/webhooks/`) — Taiga webhook receiver with HMAC signature verification, event routing (exact match, wildcard, catch-all), and parsers for user story data and status changes
- **Identity manager** (`pkg/identity/`) — on-demand creation of agent identities in Gitea and Taiga, agent lookup by ID/specialization, idle agent reuse, recovery registration
- **Assignment engine** (`pkg/assignment/`) — FIFO ticket queue with configurable max concurrency, tag-based delegation (`delegate:` / `active:` tags), escalation tracking, and delegation completion detection
- **Lifecycle manager** (`pkg/lifecycle/`) — creates/monitors/deletes agent worker K8s Jobs with full security context (non-root, no privilege escalation, capabilities dropped), configurable timeouts and retries
- **Plan workflow** (`pkg/plan/`) — parses markdown implementation plans into executable steps, tracks step status/agents/PRs, detects parallelism and specialization requirements, generates release notes
- **PR review** (`pkg/review/`) — invokes Claude Code CLI to review PR diffs, posts structured reviews on Gitea (approve/request changes/comment)
- **State manager** (`pkg/state/`) — persists orchestrator state (agents, queue, assignments, plans) to a K8s ConfigMap with optimistic locking and debounced saves
- **Notifications** (`pkg/notifications/`) — dispatches events via local webhook and desktop notifications, maintains an in-memory event log with filtering
- **Configuration** (`pkg/config/`) — YAML config with defaults, per-specialization overrides

### Building

```bash
cd orchestrator
go build -o bin/orchestrator ./cmd/orchestrator
```

### Testing

```bash
cd orchestrator
go test ./...
```

### Configuration

The orchestrator reads a `config.yaml` file. All values have sensible defaults for the local k3s environment. Example:

```yaml
gitea:
  url: "http://gitea-http.gitea.svc.cluster.local:3000"
  adminUsername: "claude"
  adminPassword: "password"

taiga:
  url: "http://taiga-gateway.taiga.svc.cluster.local:9000"
  adminUsername: "admin"
  adminPassword: "password"
  projectSlug: "dev-environment"

agents:
  maxConcurrency: 3
  idleTimeoutSeconds: 300
  taskTimeoutSeconds: 3600
  retryLimit: 2
  escalationThreshold: 2
  containerImage: "agent-worker:latest"
  specializations:
    test:
      allowedTools: ["Read", "Grep", "Bash"]
    documentation:
      allowedTools: ["Read", "Edit", "Write", "Glob", "Grep"]

notifications:
  dashboardPort: 8080
  desktopNotify: false

kubernetes:
  namespace: "agents"
  agentServiceAccount: "agent-worker"
```

### Docker

```bash
cd orchestrator
docker build -t orchestrator:latest .
```

The Dockerfile uses a multi-stage build producing a ~20 MB distroless image.

## Project Structure

```
dev-env/
├── INIT.md                      # Project specification and requirements
├── IMPLEMENTATION_PLAN.md       # Detailed implementation plan and architecture
├── README.md                    # This file
├── k8s/
│   ├── namespaces.yaml          # Kubernetes namespace definitions
│   ├── gitea/
│   │   └── values.yaml          # Gitea Helm chart values
│   └── taiga/
│       ├── configmap.yaml       # Shared configuration
│       ├── secret.yaml          # Credentials (secret key injected at setup)
│       ├── volumes.yaml         # Shared PVs/PVCs for static and media
│       ├── postgres.yaml        # PostgreSQL StatefulSet
│       ├── rabbitmq.yaml        # RabbitMQ Deployment
│       ├── back.yaml            # Taiga backend (Django API)
│       ├── async.yaml           # Taiga async worker (Celery)
│       ├── events.yaml          # Taiga events (WebSocket)
│       ├── protected.yaml       # Taiga protected media proxy
│       ├── front.yaml           # Taiga frontend (Angular SPA)
│       └── gateway.yaml         # Nginx gateway + ConfigMap
├── agent/
│   ├── Dockerfile               # Agent worker container image
│   ├── bootstrap.sh             # Agent lifecycle script
│   ├── system-prompt.md         # Claude Code system prompt template
│   └── test-bootstrap.sh        # Bootstrap logic unit tests
├── config.yaml                  # Master orchestrator configuration
├── k8s/
│   ├── agents/
│   │   ├── network-policy.yaml  # Egress restrictions for agent pods
│   │   ├── rbac.yaml            # ServiceAccounts and RBAC roles
│   │   ├── job-template.yaml    # Agent Job template + service endpoints
│   │   └── policies.yaml       # Per-specialization tool restrictions
│   ├── gitea/
│   │   ├── values.yaml          # Gitea Helm chart values
│   │   └── act-runner.yaml      # Gitea Actions runner (DinD)
│   └── ...
├── orchestrator/
│   ├── Dockerfile               # Multi-stage build (distroless)
│   ├── go.mod / go.sum          # Go module definition
│   ├── cmd/orchestrator/        # Main entrypoint
│   └── pkg/
│       ├── config/              # Configuration structs + YAML loading
│       ├── gitea/               # Gitea REST API client
│       ├── assignment/           # FIFO ticket queue + delegation engine
│       ├── identity/            # Agent identity lifecycle management
│       ├── lifecycle/           # K8s Job creation, monitoring, deletion
│       ├── notifications/       # Event dispatch (webhook, desktop) + log
│       ├── plan/                # Implementation plan parsing + workflow
│       ├── review/              # PR review via Claude Code CLI
│       ├── state/               # ConfigMap-backed state persistence
│       ├── taiga/               # Taiga REST API client
│       └── webhooks/            # Taiga webhook receiver + event routing
├── workflows/
│   ├── test.yaml                # Default test workflow (multi-language)
│   ├── pre-release.yaml         # Auto pre-release on merge to main
│   └── release.yaml             # Release on stable version tag
└── scripts/
    ├── setup.sh                 # Master setup script
    ├── teardown.sh              # Cleanup script
    ├── install-k3s.sh           # k3s installation
    ├── init-gitea.sh            # Gitea initialization
    ├── init-taiga.sh            # Taiga initialization
    ├── verify.sh                # Health checks
    └── wait-for-ready.sh        # Pod readiness helper
```
