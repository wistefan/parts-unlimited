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
docker build -t localhost:5000/orchestrator:latest . # Build container
docker push localhost:5000/orchestrator:latest       # Push to local registry
```

### Agent Worker
```bash
cd agent
docker build -t localhost:5000/agent-worker:latest . # Build container
docker push localhost:5000/agent-worker:latest        # Push to local registry
bash agent/test-bootstrap.sh             # Unit tests for bootstrap.sh
```

### System Operations
```bash
sudo ./scripts/setup.sh                  # Deploy everything (idempotent)
./scripts/verify.sh                      # Health checks
sudo ./scripts/teardown.sh               # Remove services, keep k3s
sudo ./scripts/teardown.sh --full        # Complete reset including k3s
./scripts/import-images.sh               # Build & push both images to local registry
./scripts/import-images.sh agent-worker  # Build & push single image
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
| `pkg/metrics` | Prometheus metric definitions + /metrics handler registration |

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
- Pushgateway: `http://pushgateway.monitoring.svc.cluster.local:9091` (injected into agent pods as `PUSHGATEWAY_URL`)
- Prometheus: `http://prometheus.monitoring.svc.cluster.local:9090`

External URLs: Gitea at `http://localhost:3001`, Taiga at `http://localhost:9000`, Grafana at `http://localhost:3000` (admin/password), Prometheus at `http://localhost:9090`.

## Observability

Token usage and job metrics flow through a Prometheus stack:

- Agent pods push per-turn, per-session, per-tool, and job-level gauges to the Pushgateway at the end of every session via `agent/parse-usage.py` (run from `bootstrap.sh`).
- The orchestrator exposes `/metrics` on port 8080 with job create/complete counters, job-duration histograms, a queue-depth gauge, and review-invocation counters.
- Three provisioned Grafana dashboards (under `k8s/monitoring/grafana-dashboards.yaml`) cover: Token Usage Overview, Cache Efficiency, and Tool Usage & Attribution.

Dashboards, scrape config, and pushgateway network access are deployed by `./scripts/setup.sh` (step 5 — "Monitoring"). Disable metrics push per-run by unsetting `PUSHGATEWAY_URL` in the agent environment.

### Claude Spend integration

Every agent pod writes two files into the host path `/var/lib/dev-env/claude-spend/`, mirroring exactly what `npx claude-spend` looks for under `~/.claude/`:

- `projects/<repo>/<agent>-ticket-<id>-<mode>-<ts>.jsonl` — the per-run transcript (user/assistant events with synthetic timestamps).
- `history.jsonl` — a flat, append-only index mapping each `sessionId` (= the transcript filename without `.jsonl`) to a `display` label of the form `[<mode>[ step N]] Ticket #<id>: <subject>`. Without this, claude-spend falls back to the first raw user prompt, which for agents is a long system-prompt blob and not useful as a label.

Both files accumulate across runs — nothing is ever deleted — and Claude Spend runs on the host, outside the cluster.

To analyze them:

```bash
# One-time: point claude-spend's expected paths at the agents' hostPath dir.
# Back up an existing ~/.claude/projects or ~/.claude/history.jsonl first
# if you also use Claude Code locally.
mkdir -p ~/.claude
ln -sfn /var/lib/dev-env/claude-spend/projects     ~/.claude/projects
ln -sfn /var/lib/dev-env/claude-spend/history.jsonl ~/.claude/history.jsonl

# Serve the UI (no installation needed).
npx claude-spend
```

The transcript generator lives in `agent/export-claude-spend.py`; it is invoked by `bootstrap.sh` after each Claude invocation and adds synthetic per-line timestamps, which is the only field Claude Spend's parser requires on top of raw stream-json. The `history.jsonl` line is appended in `bootstrap.sh` itself via `jq` (concurrent agents append safely — JSONL lines are well below `PIPE_BUF`).

## Agent Security Model

Agent containers run non-root (UID 1000), have no K8s API access (`automountServiceAccountToken: false`), and are restricted by NetworkPolicy to Gitea + Taiga + public HTTPS only. All capabilities are dropped. Workspace is ephemeral (`emptyDir`).

## Token Optimization Patterns

- **Rolling context summaries**: agents end Taiga comments with `### Context Summary` replacing full history
- **CLAUDE.md codebase context**: generated during plan phase, auto-loaded by Claude Code for step/fix agents
- **System prompt caching**: stable context (mode prompt + CLAUDE.md) placed in system prompt for API cache hits

## Common Troubleshooting

**ErrImagePull / registry unavailable**: Ensure the local registry pod is running (`kubectl get pod -n registry`). Re-push images with `./scripts/import-images.sh`

**Port conflicts**: Use `GITEA_PORT=3002 TAIGA_PORT=9001 sudo -E ./scripts/setup.sh`
