# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Parts Unlimited (dev-env) is a locally-hosted, Kubernetes-based platform that orchestrates Claude AI agents to autonomously perform software development work. Work is driven by Taiga project management tickets. All components run on a single machine using k3s.

## Build & Test Commands

### Orchestrator (Go)
```bash
cd orchestrator
go build -o bin/orchestrator ./cmd/orchestrator   # Build
go test ./...                                      # Run all tests
go test ./pkg/assignment                           # Test single package
go test -run TestAssignmentEngine ./pkg/assignment # Run single test
go vet ./...                                       # Static analysis
docker build -t orchestrator:latest .              # Build container
```

### Agent Worker
```bash
cd agent
docker build -t agent-worker:latest .    # Build container
bash agent/test-bootstrap.sh             # Unit tests for bootstrap.sh
```

### System Operations
```bash
sudo ./scripts/setup.sh                  # Deploy everything (idempotent)
./scripts/verify.sh                      # Health checks
sudo ./scripts/teardown.sh               # Remove services, keep k3s
sudo ./scripts/teardown.sh --full        # Complete reset including k3s
sudo ./scripts/import-images.sh          # Rebuild & import both images to k3s
sudo ./scripts/import-images.sh agent-worker  # Rebuild single image
```

### Viewing Logs
```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl logs -n agents deployment/orchestrator -f       # Orchestrator logs
kubectl logs -n agents <pod-name>                       # Agent worker logs
```

## Architecture

Three main components communicate via REST APIs and Kubernetes Jobs:

1. **Orchestrator** (`orchestrator/`) -- Go service that receives Taiga webhooks, manages a FIFO ticket queue, spawns agent worker Jobs in Kubernetes, runs automated PR reviews, and persists state to a ConfigMap.

2. **Agent Workers** (`agent/`) -- Ephemeral containers running Claude Code CLI. Each is spawned per-ticket in one of four modes: `analysis` (evaluate ticket), `plan` (create implementation plan + CLAUDE.md), `step` (implement one plan step), `fix` (address PR review comments). The `bootstrap.sh` script orchestrates the full lifecycle.

3. **Infrastructure** (`k8s/`, `scripts/`) -- Gitea (git hosting + CI/CD via Actions), Taiga (project management + webhooks), and supporting k3s manifests.

### Orchestrator Package Map

The entry point is `cmd/orchestrator/main.go` (~1600 lines) which wires everything together. Key packages:

| Package | Responsibility |
|---|---|
| `pkg/assignment` | FIFO ticket queue, tag-based delegation (`delegate:`/`active:` tags), concurrency control |
| `pkg/lifecycle` | K8s Job CRUD with security contexts, timeouts, retries |
| `pkg/identity` | On-demand agent identity creation in Gitea + Taiga |
| `pkg/state` | ConfigMap-backed persistence with optimistic locking |
| `pkg/taiga` | Taiga REST API client (stories, comments, tags, statuses) |
| `pkg/gitea` | Gitea REST API client (repos, PRs, reviews, branches) |
| `pkg/webhooks` | Taiga webhook receiver with HMAC verification and event routing |
| `pkg/review` | PR review via `claude -p` subprocess |
| `pkg/plan` | Markdown plan parsing, step tracking, release notes |
| `pkg/notifications` | Webhook + desktop notification dispatch |
| `pkg/config` | YAML config loading with defaults |

### Ticket Lifecycle & Agent Modes

```
ready --> analysis agent --> [analysis:proceed] --> plan agent --> plan PR
  --> human merges plan PR --> step agent (step 1) --> step PR
  --> human merges step PR --> step agent (step 2) --> ... --> [step:complete]
  --> ready for test
```

Fix agents are spawned when a human requests changes on any PR.

### Branch Naming Convention

- `ticket-{id}/work` -- integration branch (all step PRs merge here)
- `ticket-{id}/plan` -- plan PR branch
- `ticket-{id}/step-{n}` -- step implementation branch

### Tag-Based Delegation

- `delegate:{spec}` -- general agent requests specialized work
- `active:{spec}` -- specialist is working; removed when done

## Key Configuration

`config.yaml` at repo root configures the orchestrator. Important settings:
- `agents.maxConcurrency` (default: 3) -- max parallel agents
- `agents.taskTimeoutSeconds` (default: 3600) -- K8s Job deadline
- `agents.retryLimit` (default: 2) -- retries before escalation
- `agents.containerImage` (default: `agent-worker:latest`)

In-cluster service URLs:
- Gitea: `http://gitea-http.gitea.svc.cluster.local:3001`
- Taiga: `http://taiga-gateway.taiga.svc.cluster.local:9000`

External URLs: Gitea at `http://localhost:3001`, Taiga at `http://localhost:9000`

## Agent Security Model

Agent containers run non-root (UID 1000), have no K8s API access (`automountServiceAccountToken: false`), and are restricted by NetworkPolicy to Gitea + Taiga + public HTTPS only. All capabilities are dropped. Workspace is ephemeral (`emptyDir`).

## Token Optimization Patterns

- **Rolling context summaries**: agents end Taiga comments with `### Context Summary` replacing full history
- **CLAUDE.md codebase context**: generated during plan phase, auto-loaded by Claude Code for step/fix agents
- **System prompt caching**: stable context (mode prompt + CLAUDE.md) placed in system prompt for API cache hits

## Common Troubleshooting

**ErrImageNeverPull**: Re-import images with `docker save orchestrator:latest agent-worker:latest | sudo k3s ctr images import -`

**Port conflicts**: Use `GITEA_PORT=3002 TAIGA_PORT=9001 sudo -E ./scripts/setup.sh`
