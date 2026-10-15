# AGENTS.md

This file provides guidance to Claude Code (claude.ai/code) and other coding agents when working with code in this repository.

## Project Overview

Parts Unlimited (dev-env) is a locally-hosted, Kubernetes-based platform that orchestrates Claude AI agents to autonomously perform software development work. Work is driven by Taiga project management tickets. All components run on a single machine using k3s.

## Hard Rules

### No orchestrator-side state persistence

**Taiga and Gitea are the only sources of truth.** The orchestrator must not persist state (assignments, queue entries, PR mappings, escalation counters, etc.) to a Kubernetes ConfigMap, a local file, an in-memory cache that survives restarts, or any other store. Every decision — "is this ticket assigned?", "what mode should run next?", "which PR belongs to which ticket?" — must be derived on demand from Taiga (statuses, assignees, tags, comments), Gitea (PRs, reviews, branches), and the K8s API (Jobs).

Why: dual-write bugs. Earlier versions kept ConfigMap-backed state (`pkg/state`) and in-memory maps (`assignments`, `queue`, `prMappings`, `escalations`, `busyAgents`) alongside Taiga/Gitea; they drifted, leaving tickets that looked "assigned" internally but had no running Job, no open PR, and no human owner — silently stuck forever. Deriving state fresh from the external systems on every reconcile pass eliminates the class of bug entirely.

How to apply: when adding a feature, do not introduce any field that survives a single `reconciler.ReconcileAll` pass. Per-pass caches built and discarded inside one call are fine. New reads go through `taigaClient` / `giteaClient` / `lifecycleMgr` (the K8s wrapper). The stateless reconciler in `pkg/reconciler` is the single spawn path; webhooks call `nudgeReconciler` and return.

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

1. **Orchestrator** (`orchestrator/`) -- Go service that receives Taiga and Gitea webhooks, runs the stateless reconciler against Taiga + Gitea + K8s on every pass, and spawns agent worker Jobs in Kubernetes. Holds no cross-pass state of its own (see Hard Rules). It does not review PRs itself -- the orchestrator image has no Claude CLI; reviewing is a `review`-mode agent Job like any other work.

2. **Agent Workers** (`agent/`) -- Ephemeral containers running Claude Code CLI. Each is spawned per-ticket in one of six modes: `analysis` (evaluate ticket), `plan` (create implementation plan + AGENTS.md), `step` (implement one plan step), `onestep` (implement a whole small ticket in one PR), `fix` (address PR review comments), `review` (post an advisory senior-engineer review on a PR). The `bootstrap.sh` script orchestrates the full lifecycle.

3. **Infrastructure** (`k8s/`, `scripts/`) -- Gitea (git hosting + CI/CD via Actions), Taiga (project management + webhooks), and supporting k3s manifests.

### Orchestrator Package Map

The entry point is `cmd/orchestrator/main.go` (~1600 lines) which wires everything together. Key packages:

| Package | Responsibility |
|---|---|
| `pkg/reconciler` | Stateless decision core: derives "what to do next" for each ticket from Taiga + Gitea + K8s on every pass. Single spawn path. |
| `pkg/assignment` | Tag-based delegation helpers (`delegate:`/`active:` tag manipulation). No engine; no in-memory state. |
| `pkg/lifecycle` | K8s Job CRUD with security contexts, timeouts, retries. K8s is the source of truth for "is this agent busy" via `ListActiveAgents`. |
| `pkg/identity` | On-demand agent identity creation in Gitea + Taiga. Hydrates from Gitea on startup; no persistence. |
| `pkg/taiga` | Taiga REST API client (stories, comments, tags, statuses) |
| `pkg/gitea` | Gitea REST API client (repos, PRs, reviews, branches, user search) |
| `pkg/webhooks` | Taiga + Gitea webhook receivers; PR-body / branch-name parser for ticket-id resolution |
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

A fix agent is anchored to the PR, not to a ticket branch: it resolves
`BRANCH_NAME` from the PR's `head.ref`, commits there, pushes, and
answers on the PR itself -- a threaded reply under each inline review
comment it was given, plus one issue comment summarising the fix as a
whole. Both are plain comments (a *review* would be rejected by Gitea,
since the fix agent authored the branch). The replies are addressed by
comment id: the ids are rendered into the fix prompt alongside each
comment, the agent returns them in `pr_comment_replies` in
`completion-status.json`, and `bootstrap.sh` POSTs each to
`/pulls/{index}/comments/{id}/replies`. Ids the agent did not receive
are dropped -- Gitea comment ids are repo-global, so an invented one
could otherwise land a reply on an unrelated PR. Pushing is
also what ends the fix loop -- Gitea marks the human's `REQUEST_CHANGES`
review stale once new commits land, and `findFixTarget` only matches
non-stale ones. A fix agent that cannot resolve the head branch fails
loudly via `request_human_input` rather than cutting a branch of its own;
a guessed branch would look like success and land nothing.

### Automated PR Review

A `review`-mode agent posts an advisory review on every open ticket PR
(plan PRs included). It is bounded by three rules, and each is enforced
mechanically rather than by prompt:

- **It can only comment.** `bootstrap.sh` hard-codes `event: "COMMENT"`
  when posting; nothing the agent writes into `review-result.json`
  becomes an `APPROVED` or `REQUEST_CHANGES` review. This matters beyond
  etiquette: `REQUEST_CHANGES` is the trigger for spawning a fix agent,
  so a reviewer able to emit it could dispatch agents on its own opinion.
  Approving and merging stay with the human.
- **It reviews a revision once.** The reconciler compares the PR's head
  SHA against the reviews already on the PR, matching either Gitea's
  `commit_id` or the `<!-- agent-review: <sha> -->` marker the reviewer
  embeds in its body (`ReviewSHAMarker` in `pkg/reconciler`, mirrored in
  `bootstrap.sh`). A PR is re-reviewed only after new commits land. A PR
  whose head SHA cannot be read is skipped, never re-reviewed forever.
- **It owns nothing.** Reviewers are a separate `review-agent-N`
  identity (Gitea rejects a review by the PR's author), get `read`
  access only, run with a read-only tool set, and post no Taiga
  comment -- so they never enter a ticket's working-agent history or its
  lifecycle-marker stream.

Review is the last decision in `DeriveTicketState`, so it can only run
when no plan/step/fix work is due; it never delays implementation.
Toggle with `agents.review.enabled` in `config.yaml`.

### Credential Renewal

Agent pods mount the `claude-credentials` Secret read-only and copy it
into an `emptyDir`, so any OAuth token the Claude CLI refreshes inside a
pod is discarded when that pod exits. `scripts/setup.sh` writes the
Secret once, from the host's `~/.claude/.credentials.json`; nothing
updated it afterwards. When its access token expired, every agent failed
in well under a second with `OAuth session expired and could not be
refreshed`, produced no output, and was respawned on the next reconcile
pass indefinitely -- a review agent burned 139 runs on one PR this way,
because `maxAgentReviewsPerPR` counts *posted* reviews and a run that
posts nothing never increments it.

`pkg/credentials` closes that. The orchestrator checks the Secret every
`agents.credentials.checkIntervalSeconds` and performs the OAuth
`refresh_token` grant when the token is within
`refreshBeforeExpirySeconds` of expiring, writing the result back to the
Secret. Three details matter:

- **One refresher, not N.** Refresh tokens rotate, so pods refreshing
  independently meant the first one consumed the stored token and
  invalidated it for every other pod. Agents are now pure consumers.
- **Unmodelled fields round-trip.** The document is decoded with
  `UseNumber()` into a generic map and only the fields the token
  response speaks to are replaced, so fields this code does not model
  survive and millisecond epochs stay integer literals rather than being
  rewritten into exponent notation.
- **A dead refresh token fails loudly** with the operator action
  (`claude login`, then re-run `scripts/setup.sh`) instead of retrying a
  token that can never be exchanged.

This persists a credential, not state, so it does not breach the Hard
Rule above: it caches nothing across calls and answers no question about
a ticket. Toggle with `agents.credentials.enabled`; an API-key setup is
skipped automatically.

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
- `agents.model` / `agents.effort` (defaults: `claude-opus-5-5`, `high`) --
  model and effort for the implementing agents: analysis, plan, step,
  onestep, fix
- `agents.review.model` / `agents.review.effort` (same defaults) -- model and
  effort for review agents

Reviewers keep their own keys even while the values match the implementing
agents': reviewing judges a changeset it cannot compile or test, and moving
the reviewers alone — by ConfigMap edit, with no image rebuild — is the point
of the split. Nothing in the code assumes the two stay equal.

All four values are passed straight to the Claude CLI as `--model` /
`--effort`. A model name the CLI does not recognise is not a startup error:
every agent exits in under a second with `There's an issue with the selected
model`, writes nothing, and is respawned on the next reconcile pass
indefinitely. The dotted spelling `claude-opus-4.8` cost ~40 no-op runs in
ten minutes this way.

Effort (`low|medium|high|xhigh|max`) buys thinking tokens, which land in
output. Measured over a week of real runs, output is **0.2%** of token spend
against **99.8%** in cached context, so raising effort costs almost nothing
directly -- what it can cost is longer runs, since every added turn re-reads
the context. Judge a change by run length in the claude-spend data, not by
output tokens.

In-cluster service URLs:
- Gitea: `http://gitea-http.gitea.svc.cluster.local:3001`
- Taiga: `http://taiga-gateway.taiga.svc.cluster.local:9000`

External URLs: Gitea at `http://localhost:3001`, Taiga at `http://localhost:9000`.

## Observability

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
- **AGENTS.md codebase context**: generated during plan phase, auto-loaded by Claude Code for step/fix agents
- **System prompt caching**: stable context (mode prompt + AGENTS.md) placed in system prompt for API cache hits

## Common Troubleshooting

**ErrImagePull / registry unavailable**: Ensure the local registry pod is running (`kubectl get pod -n registry`). Re-push images with `./scripts/import-images.sh`

**Port conflicts**: Use `GITEA_PORT=3002 TAIGA_PORT=9001 sudo -E ./scripts/setup.sh`
