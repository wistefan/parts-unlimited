# No-State Orchestrator Implementation Plan

## 1. Motivation

The hard rule in `CLAUDE.md` is unambiguous: **Taiga and Gitea are the only sources of truth**. The orchestrator must derive every per-ticket decision from those two systems on demand, never from persisted or in-memory state that crosses reconcile passes.

The codebase only partially honours that rule. The proximate evidence is **the ticket #8 incident**:

- Ticket #8 finished an `analysis` job with `[analysis:proceed]` posted on Taiga.
- `handleAnalysisCompletion` (`orchestrator/cmd/orchestrator/main.go:1058-1066`) called `respawnAgent(ticketID, "plan")`.
- `respawnAgent` → `pickAgentForTicket` → `pickAgentForTicketWithBusy(ticketID, o.assignEngine.GetBusyAgents())` (`main.go:1252-1254`).
- The in-memory `busyAgents` map still listed `general-agent-1` because the analysis-completion path **never releases it**: `handleJobCompletion`'s `case "analysis":` branch invokes `handleAnalysisCompletion`, which calls `CompleteTicket` only on the `need-info` / `onestep-rejected` / default paths but **not** on `proceed` / `onestep-proceed` (`main.go:1057-1086`). Only `WaitForPR` releases the agent — and `WaitForPR` is only called for plan/step/fix modes (`main.go:1006-1015`).
- Result: `pickAgentForTicketWithBusy` saw `general-agent-1` as busy and returned `errPreviousAgentBusy`. The plan agent never spawned.
- The K8s-derived busy set (`lifecycleMgr.ListActiveAgents`) used by the new stateless reconciler would correctly have shown no busy agent — the analysis Job had already terminated.

This is the canonical dual-write drift the hard rule warns about: two views of "is agent X busy?" exist, they disagree, and the legacy view wins on the path that needs it most.

The fix is not "release the agent in the analysis-proceed branch". The fix is to delete the second view: the `busyAgents` map should not exist; busy status is a property of the K8s API, period. This plan removes that map and every other piece of cross-pass orchestrator state, in steps that are individually shippable.

## 2. Audit findings

All findings are categorized as **C** (cross-pass state — must be removed) or **P** (per-pass cache, allowed). File:line numbers reference the working tree at `topic/reduce-step-token-usage` (commit `d57b99c`).

### 2.1 ConfigMap-backed state (`pkg/state`) — C

**File:** `orchestrator/pkg/state/manager.go` (full file)

**Schema** (`OrchestratorState`, lines 30-37):

| Field | Source of truth replacement |
|---|---|
| `Agents []AgentIdentity` | Gitea users (`giteaClient.GetUser`) + Taiga project membership (`taigaClient.ListProjectMembers`). Already done by `identity.AdoptExisting` (`pkg/identity/manager.go:193`). |
| `Queue []QueueEntry` | `taigaClient.ListUserStories(projectID, {StatusID: ready})` — already done by `reconciler.listActionableStories` (`pkg/reconciler/shadow.go:185-198`). |
| `Assignments map[int]*TicketAssignment` | (a) "is ticket assigned to an agent?" → has Job for ticket via `lifecycleMgr.HasJobForTicket`. (b) "which agent?" → `findLastAgentForTicket(ticketID)` from Taiga comment history (`main.go:1221-1232`). (c) "delegated to whom?" → derived from `active:` tags on the Taiga ticket. (d) "status" → derived from comments + Job state via `DeriveTicketState`. |
| `Escalations map[int]*EscalationEntry` | Effectively dead — see 2.2 below. If reintroduced, derive count from Gitea PR list filtered by branch prefix `ticket-{id}/` and `state=closed && !merged`, or from Taiga comments containing `[pr-rejected:N]`. |
| `PRMappings map[string]int` | Already redundant with the branch-prefix scan (`giteaClient.ListPullRequestsForTicket`, `pkg/gitea/client.go:184-210`) and `webhooks.ParseTicketIDFromPRBody` (`pkg/webhooks/gitea_handler.go:218-233`). |
| `LastSaved time.Time` | Trivially gone with the schema. |

**Writers (`saveState` callers in `cmd/orchestrator/main.go`):**

- `main.go:223` (shutdown), `:511` (PR opened mapping write), `:617` (PR closed escalate cleanup), `:699` (review-request-changes spawn), `:754` (transitionToReadyForTest), `:1014` (handleJobCompletion non-analysis), `:1087` (handleAnalysisCompletion), `:1299` (spawnAgentForTicket), `:1348` (respawnAgent), `:1497` (spawnFixAgent), `:1662` (assignTicket), `:1701` (handleDelegation), `:1997` (spawnForReconciler — even the new path still writes!).

  Plus the implementation at `main.go:2082-2099` constructs a fresh snapshot from `identityMgr.ListAgents()`, `assignEngine.GetQueue()`, `assignEngine.GetAllAssignments()`, `o.prMappings` and persists via `stateMgr.Save`. After step 2 below, none of these reads will exist; saveState becomes a no-op and is deleted in step 3.

**Readers (`Load` / `RegisterExisting` / `RestoreAssignment`):**

- Single reader: `initialize` (`main.go:346-367`). Pulls saved state on startup, registers each agent into `identityMgr`, copies `prMappings`, and replays each assignment via `assignEngine.RestoreAssignment`. After step 1 (cutover) the reconciler does its first pass without needing any of this; after step 2 the readers are gone.

**RBAC:** `k8s/agents/rbac.yaml:41-44` grants the orchestrator ConfigMap CRU permissions specifically to support `pkg/state`. The ConfigMap itself (`orchestrator-state` in the `agents` namespace) has no manifest — it is created on first `Save`. The RBAC rule must be removed in step 9 along with the package.

### 2.2 In-memory maps in `pkg/assignment/Engine` — C

**File:** `orchestrator/pkg/assignment/engine.go`

#### 2.2.1 `assignments map[int]*TicketAssignment` (line 80)

**Writers:**
- `AssignAgent` (`engine.go:149-172`) — called from `main.go:657, 667, 1274, 1323, 1462, 1629, 1958`. Each spawn site writes here.
- `RestoreAssignment` (`engine.go:178-192`) — called from `main.go:362` on startup.
- `RecordDelegation` (`engine.go:204-223`) — called from `main.go:1679`.
- `WaitForPR` (`engine.go:283-297`) — called from `main.go:1013`.
- `CompleteTicket` (`engine.go:260-276`) — called from `main.go:546, 616, 753, 1025, 1073, 1080, 1084, 1636, 1732, 1737, 1779, 1857`.
- `CompleteDelegation` (`engine.go:227-257`) — never called from `main.go` (dead).

**Readers:**
- `GetAssignment` (`engine.go:342-353`) — called from `main.go:448, 464, 540, 655, 1714, 1799` and indirectly from `getAllAssignments`. **Every one of these readers maps directly to a Taiga or K8s lookup:**
  - `main.go:448, 464` (webhook): `if o.assignEngine.GetAssignment(data.ID) == nil` → in authoritative mode the branch is already dead (the `if !o.legacyReconcileActive` guard above it returns first). After step 1 these whole branches are deleted.
  - `main.go:540` (PR-merged dedup): `if existing := o.assignEngine.GetAssignment(ticketID); existing != nil && existing.Status == "assigned"` → replace with `lifecycleMgr.HasJobForTicket(ctx, ticketID)`. The semantic change is harmless: "another webhook already kicked off a job for this ticket" is exactly what HasJob says.
  - `main.go:655` (review-request-changes spawn): same — already covered by the `legacyReconcileActive` guard at line 644 and deleted in step 1.
  - `main.go:1714, 1799` (legacy `reconcile`): the whole legacy `reconcile` is deleted in step 1.
- `GetAllAssignments` (`engine.go:380-391`) — called from `main.go:1723, 2093`. Deleted with legacy `reconcile` and `saveState`.

**Replacement summary:** "is this ticket currently being worked on?" = `lifecycleMgr.HasJobForTicket(ctx, ticketID)`. "Which agent?" = `findLastAgentForTicket` (already exists, reads Taiga). "What status / waiting on what?" = `reconciler.DeriveTicketState`.

#### 2.2.2 `busyAgents map[string]int` (line 82)

**Writers:** `AssignAgent`, `RestoreAssignment`, `RecordDelegation`, `CompleteDelegation`, `CompleteTicket`, `WaitForPR` — same lines as above (this map is updated as a side effect of every assignment-map mutation).

**Readers:**
- `Dequeue` (`engine.go:131-134`) — concurrency cap check; called from `processQueue` (`main.go:1595`). Deleted with the legacy queue in step 4.
- `GetBusyAgents()` (`engine.go:330-339`) — called from `main.go:662, 1253, 1673` and from `pickAgentForTicket` (`main.go:1253`). Each call site:
  - `main.go:662` (review handler legacy path) — deleted in step 1.
  - `main.go:1253` (`pickAgentForTicket` legacy wrapper) — `pickAgentForTicket` is itself only called from `respawnAgent`, `spawnAgentForTicket`, and `spawnFixAgent`, which after step 1 only run via the reconciler's authoritative spawner. The reconciler already passes a K8s-derived busy set to `pickAgentForTicketWithBusy` directly (`main.go:1925-1927, 1950, 2013-2034`). The wrapper and its `GetBusyAgents()` call are deleted in step 2.
  - `main.go:1673` (`handleDelegation`) — replace with `lifecycleMgr.ListActiveAgents(ctx)`.
- `ActiveCount()` (`engine.go:362-367`) — never called outside tests. Dead.

**This is the source of the ticket #8 bug.** Step 2 is the canonical fix.

#### 2.2.3 `queue []QueueEntry` (line 79)

**Writers:** `Enqueue` (`engine.go:98-119`) — called from `main.go:450, 466, 492, 1606, 1716`. **All five sites are inside `if o.legacyReconcileActive` branches** (or inside legacy `reconcile`/`processQueue`). Already dead in authoritative mode.

**Readers:** `Dequeue` (`main.go:1595` only), `QueueLength()` (`main.go:165` for metrics + `engine.go:362` ActiveCount/etc.), `GetQueue()` (`main.go:2092` for saveState).

**Replacement:** the queue is `taigaClient.ListUserStories(projectID, {StatusID: ready})` filtered by "no Job exists for this ticket". The reconciler already does this (`pkg/reconciler/shadow.go:185-198`, `reconciler.go:151-156` enforces the cap). Step 4: delete; have `metrics.RegisterQueueDepth` call a Taiga-derived counter (count ready tickets minus those with active Jobs).

#### 2.2.4 `escalations map[int]*EscalationEntry` (line 81)

**Writers:** `RecordReassignment` (`engine.go:301-320`), `CompleteTicket` (line 273), `ResetEscalation` (`engine.go:323-327`).

**Readers:** none in `main.go` — only the engine itself and tests. The `escalationThreshold` config field is never consulted on a live code path.

**Status: dead code.** Removed wholesale in step 6 with no replacement; the unused `EscalationThreshold` config field can stay or go in the same step.

### 2.3 Other in-memory state in `cmd/orchestrator/main.go`

#### 2.3.1 `o.prMappings map[string]int` (line 82) — C

**Writer:** `main.go:510` (PR opened webhook), persisted via `saveState` at `:511`. Loaded from `pkg/state` at `:355-357`.

**Reader:** `resolveTicketFromPR` (`main.go:816-822`).

**Replacement:** `webhooks.ParseTicketIDFromPRBody(event.PullRequest.Title, event.PullRequest.Body)` (`pkg/webhooks/gitea_handler.go:218`) already parses `Ticket #N` and `ticket-N/` from title/body; the latter matches the branch prefix every orchestrator-managed PR uses. `resolveTicketFromPR` becomes a one-liner that delegates to it. Removed in step 7.

#### 2.3.2 `o.registeredRepos map[string]bool` (line 86) — C (per-process, not per-pass)

**Writer/reader:** `main.go:771, 791` in `ensureGiteaWebhook`. Survives the whole process lifetime; not persisted.

**Replacement:** Use `giteaClient.ListRepoWebhooks(owner, repo)` (does not exist yet — see Open Question 4) to check for an existing hook by URL+events match before creating, then drop the in-memory cache. Acceptable cost: one extra API call per PR-opened on a repo we have already hooked, only on first encounter. Alternative: leave this one in place — it only saves API calls, doesn't drift, and is rebuilt per process. **Recommendation:** keep it but rename to `seenWebhookRepos` and document it as "best-effort same-process dedup, not state". Decision deferred to Open Question 4.

#### 2.3.3 `lifecycle.Manager.activeJobs map[string]string` (line 108) — C

**Writer:** `CreateJob` (`pkg/lifecycle/manager.go:378-380`). **Reader:** `DeleteJob` (`:402-404`), `GetActiveJobNames` (`:605-614`). `GetActiveJobNames` has **no callers** in the codebase — dead. The map is only used to know which Jobs were created in this process; everything authoritative reads from K8s via `ListActiveJobs`. Removed in step 8.

#### 2.3.4 `lifecycle.Manager.tracking map[string]trackedJob` (line 109) — P

This is metrics-only state. It records mode/specialization/start-time at Job creation so completion metrics can be emitted with consistent labels (`pkg/lifecycle/manager.go:127-152`). It does survive across reconcile passes within one process, and it does drift on orchestrator restart (a Job created before restart will not produce completion metrics after restart). That is an acceptable miss for metrics. Document it. Not removed.

#### 2.3.5 `identity.Manager.agents` and `.counts` (manager.go:58-59) — C

**Writers:** `createAgentLocked` (`:144`), `RegisterExisting` (`:277-291`), `AdoptExisting` (`:227-231`), `RemoveAgent` (`:298`).

**Readers:** `GetOrCreateAgent` (`:89-93`, idle-agent reuse loop), `GetAgent` (`:151-155`), `ListAgents` (`:158-167`), `ListBySpecialization`.

**Drift surface:** `agents` cached after a successful `createAgentLocked` is identical to what's already in Gitea + Taiga (the function creates idempotently by looking up Gitea first). `counts` is maintained to avoid creating `general-agent-1` twice — but `createAgentLocked` already calls `giteaClient.GetUser(id)` first and reuses the existing user if it exists, so duplicate-creation is already prevented by Gitea's username uniqueness. The map is purely a cache.

**Replacement plan:** keep `agents` as a per-pass cache (rebuild on `Manager` construction by listing Gitea users matching `*-agent-*` and hydrating). Keep `counts` derived the same way. After this change, a fresh process derives the registry from Gitea+Taiga at startup — equivalent to the current behaviour after `loadState` is removed in step 2. **Net effect:** the registry is rebuilt fresh per process; nothing is persisted between restarts. This is allowed under the hard rule (per-process is a long-lived per-pass cache; the behaviour is no different from running the orchestrator for the first time). Keep `RegisterExisting` deleted (no longer called) and `AdoptExisting` as the single recovery path. Step 5.

### 2.4 Webhook deduplication

The PR-closed handler at `main.go:582-590` reads Taiga comments and looks for the ticket-specific marker `[pr-rejected:{prNum}]`. This is **already** Taiga-derived dedup, not in-memory dedup — exactly the pattern the rule prescribes. **No change needed.**

The PR-merged handler at `main.go:540-543` does in-memory dedup via `assignEngine.GetAssignment(ticketID).Status == "assigned"`. After step 2 this becomes `lifecycleMgr.HasJobForTicket(ctx, ticketID)`. The semantic change: instead of "another webhook already updated my in-memory state", we ask "is a Job already running for this ticket". That is the more reliable check (it survives orchestrator restart, K8s is the source of truth) and matches what the reconciler already does (`pkg/reconciler/shadow.go:238-256`).

The review-request-changes handler at `main.go:639-642` already uses `lifecycleMgr.HasJobForTicket` — Taiga/K8s-derived. **No change needed.**

### 2.5 Identity manager registry

Covered in 2.3.5. Conclusion: rebuild on construction by enumerating Gitea users; no cross-restart persistence required, and the registry is reconstructible from Gitea + Taiga alone. The current code already proves this via `AdoptExisting`.

### 2.6 Summary of "must remove" items

| # | Where | What | Replacement |
|---|---|---|---|
| 1 | `pkg/state/*` | Whole package | Delete |
| 2 | `assignment.Engine.busyAgents` | Map | `lifecycleMgr.ListActiveAgents` |
| 3 | `assignment.Engine.assignments` | Map | `HasJobForTicket` + `findLastAgentForTicket` + `DeriveTicketState` |
| 4 | `assignment.Engine.queue` | Slice | `taigaClient.ListUserStories(ready)` |
| 5 | `assignment.Engine.escalations` | Map | Delete (dead code) |
| 6 | `o.prMappings` | Map | `webhooks.ParseTicketIDFromPRBody` |
| 7 | `lifecycle.Manager.activeJobs` | Map | `ListActiveJobs` (already exists) |
| 8 | `legacy reconcile()` + `processQueue` + `reconcileLoop` | ~270 lines | `reconciler.ReconcileAll` |
| 9 | `pkg/state` ConfigMap RBAC | k8s/agents/rbac.yaml lines 41-44 | Delete |

## 3. Target architecture

```
                    +------------------+
                    |   Taiga (truth)  |  ticket status, assignees, comments, tags
                    +--------+---------+
                             |
                    +------------------+
                    |   Gitea (truth)  |  PRs, reviews, branches, users
                    +--------+---------+
                             |
                    +------------------+
                    |  K8s API (truth) |  Jobs (active/succeeded/failed)
                    +--------+---------+
                             |
                             v
              +-----------------------------+
              |  reconciler.ReconcileAll    |  pure decision: DeriveTicketState
              |  - 1 fetch of all stories   |  per pass
              |  - per-ticket fetch:        |
              |    comments + PRs           |
              |    + reviews on open PRs    |
              |  - in-pass cache OK         |
              |  - NO package-level vars    |
              +-------------+---------------+
                            |
                            v
                        +-------+
                        | Spawn |  -> CreateJob (K8s)
                        +-------+
```

**Allowed state:**
- A `map[int]*StorySnapshot` built inside one `ReconcileAll` invocation and discarded when it returns.
- Promise/channel-style coordination state inside a goroutine (e.g. `nudgeReconciler` fire-and-forget).
- Metrics in-memory counters (Prometheus client lib).
- Deterministic caches that are reconstructed on every process start from external systems (e.g. `identity.Manager.agents` rebuilt by listing Gitea users at NewManager time).

**Forbidden state:**
- Any `var` at package or struct level holding ticket/PR/agent business data that is mutated by request handlers and read by the reconciler.
- Anything written to a ConfigMap, file, etcd, etc.
- Any in-memory map keyed by `ticketID` / `agentID` / `prKey` whose lifetime exceeds one `ReconcileAll` call.
- "Optimisation" caches with TTLs that are not `0` (i.e. anything that survives one pass).

**Single writer:** `reconciler.ReconcileAll` and `reconciler.ReconcileTicket` are the only paths that call `Spawner.Spawn`. Webhooks become pure "nudge the reconciler" calls — `nudgeReconciler` (`main.go:2040-2049`) is the canonical pattern; every webhook handler becomes that one-liner.

## 4. Step-by-step migration

Each step is one PR. The codebase compiles and tests pass between every step.

### Step 1 — Cut over to authoritative reconciler mode

**Goal:** make the stateless reconciler the only spawn path. Delete the legacy `reconcile`/`reconcileLoop`/`processQueue` and the `legacyReconcileActive` branches in webhook handlers.

**Files touched:**
- `cmd/orchestrator/main.go` (heavy: delete legacy reconcile + processQueue + the `if !o.legacyReconcileActive { ... }` shortcuts in webhook handlers; flip `cfg.Agents.ReconcilerMode == ""` default to `authoritative` in `DefaultConfig` and in `k8s/agents/orchestrator.yaml`).
- `pkg/config/config.go` (default `ReconcilerMode: "authoritative"` in `DefaultConfig`).
- `k8s/agents/orchestrator.yaml` (set `agents.reconcilerMode: authoritative` in the inline `config.yaml`).

**Removes / replaces:**
- Delete `orchestrator.reconcile` (`main.go:1704-1870`), `reconcileLoop` (`:1872-1887`), `processQueue` (`:1592-1610`), `assignTicket` (`:1612-1665`), `pickAgentForTicket` wrapper (`:1252-1254` — `pickAgentForTicketWithBusy` stays).
- Delete the entire `if o.legacyReconcileActive { ... }` body in each webhook handler (`main.go:442-453`, `:458-472`, `:485-495`, `:540-563`, `:644-702`). Each handler's body collapses to `o.nudgeReconciler(ticketID); return nil`. The PR-closed escalation comment block at `main.go:567-620` stays — it does Taiga writes (escalation comment, assign-to-human) that are not the reconciler's job, but the `assignEngine.CompleteTicket` and `saveState` lines at `:616-617` are removed.
- Delete the `legacyReconcileActive` field and the conditional branch in `initialize` (`main.go:393-406`); always construct `reconcilerSvc` in authoritative mode.
- Remove the startup `if orch.legacyReconcileActive { orch.reconcile(ctx) }` block (`:157-161`) and the `go orch.reconcileLoop(ctx)` (`:188`).
- The `runReconcilerLoop` is unconditional now.

**Verify:**
- `go vet ./...`, `go test ./...` — including all reconciler unit tests.
- Manual: kill orchestrator pod, watch ticket #8 reproduction (analysis ends with `[analysis:proceed]`); plan agent must now spawn within one reconcile interval (30 s).
- Manual: open and merge a step PR; new step agent must spawn.
- Manual: human requests changes on a PR; fix agent must spawn.

**Risk and rollback:**
- Highest-risk step. The reconciler's behaviour is supposed to match the legacy loop exactly (`pkg/reconciler/reconciler.go:163-200` mirrors the legacy decision tree), but there is no end-to-end test fixture proving every transition.
- **Mitigation:** ship after one full week of `reconcilerMode: shadow` in the target cluster. Compare every shadow `[reconciler] SPAWN ...` log line against the legacy `Reconcile: ... spawning` log line; they must agree on action and mode for every ticket. Catalogue any mismatch before flipping to authoritative.
- Rollback: revert the PR. State is unaffected — `pkg/state` is still being written (saveState calls remain inside the surviving spawn paths until step 2).

### Step 2 — Replace `assignEngine.busyAgents` reads with K8s-derived reads (fixes ticket #8)

**Goal:** every busy-agents query goes through `lifecycleMgr.ListActiveAgents`. After this step, `busyAgents` is write-only and the bug class is gone.

**Files touched:**
- `cmd/orchestrator/main.go` lines `:662, 1253, 1673`.
- `pkg/assignment/engine.go` — `GetBusyAgents()` and `busyAgents` field stay for now (still written by `AssignAgent` / `CompleteTicket`); they become unread.

**Removes / replaces:**
- `main.go:662` already deleted in step 1 (review-request-changes legacy branch).
- `main.go:1253` (`pickAgentForTicket`): replace `o.assignEngine.GetBusyAgents()` with `lifecycleMgr.ListActiveAgents(ctx)`. The function gains a `ctx context.Context` parameter and a returnable error from `ListActiveAgents`. All callers (`spawnAgentForTicket:1264`, `respawnAgent:1312`, `spawnFixAgent:1454`) propagate the context.
- `main.go:1673` (`handleDelegation`): same — `o.identityMgr.GetOrCreateAgent(specialization, busy)` where `busy, _ := o.lifecycleMgr.ListActiveAgents(ctx)`.
- The `spawnForReconciler` path (`main.go:1933`) already receives the K8s-derived busy set from the reconciler — no change.

**Verify:**
- Reproduce ticket #8: run analysis on a ticket, watch it complete with `[analysis:proceed]` and the analysis Job terminate. The reconciler's next pass must spawn the plan agent (not blocked by phantom busy state).
- Add unit test in `pkg/lifecycle` verifying `ListActiveAgents` returns `{agentID: true}` only for Jobs with `Active > 0`.

**Risk and rollback:**
- Low. `ListActiveAgents` is already the source of truth in authoritative-mode reconciler passes.
- Rollback: revert.

### Step 3 — Delete `pkg/state` and the `saveState`/`loadState` paths

**Goal:** stop persisting to ConfigMap. Bring up cleanly with no `orchestrator-state` ConfigMap present.

**Files touched:**
- `cmd/orchestrator/main.go`:
  - Delete the `stateMgr` field (`:68`), its construction (`:333`), and the entire `loadState` / restore block at `:345-367`.
  - Delete `saveState` (`:2082-2099`) and remove every `o.saveState(...)` call (lines `:223, 511, 617, 699, 754, 1014, 1087, 1299, 1348, 1497, 1662, 1701, 1997` — most already removed by steps 1-2; verify the remaining ones).
  - Remove the `pkg/state` import.
- `pkg/state/manager.go`, `pkg/state/manager_test.go`: delete files; delete the package directory.
- `pkg/identity/manager.go`: delete `RegisterExisting` (lines 271-291) and its test in `manager_test.go:193-202`. Add `HydrateFromGitea(ctx)` (or similar) called once from `NewManager` that lists Gitea users matching the agent-id pattern and pre-populates `agents` + `counts` so `GetOrCreateAgent`'s idle-agent-reuse loop works on first call. (Functionally identical to the current `loadState→RegisterExisting` chain, except sourced from Gitea instead of the ConfigMap.)
- `pkg/assignment/engine.go`: delete `RestoreAssignment` (`:178-192`) — it has no callers after step 3.

**Verify:**
- `kubectl delete configmap -n agents orchestrator-state` and restart the pod. The orchestrator must come up cleanly. The reconciler's first pass must reproduce the running cluster's state without any state file.
- `go test ./...` (after the package deletion, the `pkg/state` test goes with the package).

**Risk and rollback:**
- After this PR ships, any existing `orchestrator-state` ConfigMap is unread; if it exists in the target cluster, leave it alone (deleted in step 9 along with the RBAC).
- Rollback: revert. The next start-up of the previous binary will re-create the ConfigMap and resume the dual-write behaviour.

### Step 4 — Delete the FIFO queue and `processQueue`

**Goal:** remove `assignEngine.queue`. The reconciler is already using Taiga's `ListUserStories(ready)` as the queue.

**Files touched:**
- `cmd/orchestrator/main.go`: `processQueue` already deleted in step 1. The remaining queue references are the metrics callback at `:165` (`metrics.RegisterQueueDepth(orch.assignEngine.QueueLength)`).
- `pkg/assignment/engine.go`: remove `queue`, `Enqueue`, `Dequeue`, `QueueLength`, `GetQueue`, `QueueEntry`.
- `pkg/metrics/metrics.go`: change `RegisterQueueDepth` to take a `func() int` that the orchestrator implements as: count of Taiga stories in `ready` status with no Job per `lifecycleMgr.HasJobForTicket`. Acceptable cost: one Taiga list + N HasJob calls per scrape, but `/metrics` is scraped at most every 15 s. If that's too expensive, expose a sampled gauge updated by the reconciler at the end of each `ReconcileAll` pass.

**Verify:**
- `go test ./...`.
- Hit `/metrics` and confirm `orchestrator_queue_depth` reports a sane value reflecting actual Taiga state.

**Risk and rollback:**
- Low, isolated to the assignment engine.

### Step 5 — Replace `assignEngine.assignments` with derivations

**Goal:** delete the `assignments` map. Every call site that read it now reads K8s + Taiga directly.

**Files touched:**
- `cmd/orchestrator/main.go`:
  - `main.go:540-543` (PR-merged dedup): `if hasJob, _ := o.lifecycleMgr.HasJobForTicket(ctx, ticketID); hasJob { return nil }`. Already exists at `main.go:639-642` for the review handler — same pattern.
  - All `o.assignEngine.AssignAgent` calls at the spawn sites (`:1274, 1323, 1462, 1629, 1958`): delete. Job creation alone is the assignment; once the Job exists, `HasJobForTicket` returns true.
  - All `o.assignEngine.CompleteTicket` calls (`:546, 616, 753, 1025, 1073, 1080, 1084, 1636`): delete. The Job's eventual transition to Succeeded/Failed is the completion signal; `ReapFinishedJobs` cleans it up; the next reconcile pass derives the next decision from comments + PR state.
  - `o.assignEngine.WaitForPR(ticketID)` (`:1013`): delete. "Waiting for PR" is just "no Job exists, but `DeriveTicketState` returns `ActionNone` because comments say plan-created/step-in-progress/fix-applied". The reconciler already handles this (`reconciler.go:225-329`).
- `pkg/assignment/engine.go`: delete `assignments`, `TicketAssignment`, `AssignAgent`, `CompleteTicket`, `WaitForPR`, `GetAssignment`, `GetAllAssignments`, `RecordDelegation`, `CompleteDelegation`. (`DelegateToSpecialization` is a pure helper kept; the tag-helpers `ExtractDelegationTags` etc. stay too.)

**Verify:**
- `go test ./pkg/reconciler/...` — the reconciler tests already exercise the decision tree.
- Manual: complete a ticket end-to-end (analysis → plan → 3 step PRs → ready-for-test). Each transition must spawn the next agent within one reconcile interval.
- Manual: kill the orchestrator pod mid-flight, restart. The new pod's first pass must resume work where the previous left off — same agent, same mode, same fork.

**Risk and rollback:**
- Medium. The deduplication semantics are slightly different (Job-existence vs. assignment-status). For PR-merged in particular, the time window between "Job created for next step" and "agent posts first comment" is the new dedup window. K8s Job creation is atomic and `HasJobForTicket` reads the API directly, so duplicate spawns from a re-delivered webhook are still impossible.
- Rollback: revert.

### Step 6 — Delete escalations dead code

**Goal:** remove `RecordReassignment` / `ResetEscalation` / `escalations` and the `EscalationThreshold` config field if not otherwise used.

**Files touched:**
- `pkg/assignment/engine.go`: delete `escalations`, `EscalationEntry`, `RecordReassignment`, `ResetEscalation`. The `escalationThreshold` field on `Engine` and the `NewEngine` parameter go too.
- `pkg/config/config.go`: remove `EscalationThreshold` from `AgentsConfig` (and `DefaultConfig`).
- `cmd/orchestrator/main.go:319`: `assignEngine := assignment.NewEngine(cfg.Agents.MaxConcurrency)` — drop the second arg.

**Verify:** `go vet`, `go test ./...`.

**Risk:** trivial. Confirmed dead via grep.

### Step 7 — Delete `prMappings`

**Goal:** remove the in-memory PR-to-ticket map.

**Files touched:**
- `cmd/orchestrator/main.go`:
  - Delete the `prMappings` field (`:82`), the load-from-state block (`:350-357` — already gone after step 3), the write at `:510`, and the read in `resolveTicketFromPR` (`:818-820`).
  - `resolveTicketFromPR` becomes: `return webhooks.ParseTicketIDFromPRBody(event.PullRequest.Title, event.PullRequest.Body)`.
- `pkg/state/manager.go` is already gone.

**Verify:** open a PR via the agent path; the PR-merged webhook must still resolve to the right ticket and trigger a reconcile nudge. The agent's PR body always embeds `Ticket #<id>` — confirmed by grepping the agent code paths (`agent/`).

**Risk:** if any agent ever opens a PR without a `Ticket #N` or `ticket-N/` reference, the webhook can't resolve it. Audit `agent/bootstrap.sh` and `agent/` PR-creation paths to confirm the convention is always followed. Open Question 1.

### Step 8 — Shrink `lifecycle.Manager`

**Goal:** delete `activeJobs map`, `GetActiveJobNames`. `tracking` stays (per-process metrics state).

**Files touched:**
- `pkg/lifecycle/manager.go:107-110`: drop `activeJobs map[string]string`. `GetActiveJobNames` (`:605-614`) — confirmed zero callers — deleted.
- Lines `:378-380, :402-404`: delete the writes/reads.

**Verify:** `go vet`, `go test ./...`.

**Risk:** trivial.

### Step 9 — Remove the now-empty `assignment.Engine`, ConfigMap RBAC, and final cleanup

**Goal:** delete or shrink `pkg/assignment` to its surviving pure helpers. Remove ConfigMap permissions from RBAC.

**Files touched:**
- `pkg/assignment/engine.go`: at this point the file contains only `DelegateTagPrefix`, `ActiveTagPrefix`, `TicketInfo`, the `AgentAssigner`/`AgentInfo`/`TicketUpdater` interfaces (also possibly unused — verify), `DelegateToSpecialization` (helper), and the `Extract*` / `Replace*` / `Has*` / `FormatEscalationComment` tag helpers. Decision: rename the package to `pkg/tags` (only purpose now is tag manipulation), or move the helpers into `pkg/reconciler` if they're only used by the delegation path which itself may move.
- `cmd/orchestrator/main.go`: `assignEngine := assignment.NewEngine(...)` line is gone; remove the field, remove the import. `handleDelegation` uses the tag helpers directly.
- `k8s/agents/rbac.yaml:41-44`: delete the ConfigMap RBAC rule. The orchestrator no longer needs configmap CRU.
- Optional: post-deploy, delete the leftover `orchestrator-state` ConfigMap manually — `kubectl delete configmap -n agents orchestrator-state`. Document in PR description.

**Verify:**
- `go vet`, `go test ./...`.
- After deploying the RBAC change, `kubectl auth can-i create configmaps -n agents --as=system:serviceaccount:agents:orchestrator` returns `no`.
- Orchestrator logs show no permission errors.

**Risk:** if step 7 missed anything, this surfaces as a compile error.

### Step ordering rationale

Step 1 (cutover) is first because steps 2-9 are individually pointless without it: in shadow / legacy-only mode the in-memory state is still authoritative, so removing it would break the running system. Once the reconciler is the only writer, every subsequent step is a pure cleanup of dead-but-still-present scaffolding.

Step 2 is explicitly second because it's the **ticket-#8 fix** — the dual-view bug is gone after that PR, even if the rest of the cleanup takes another month.

Steps 3-9 can ship in any order after step 2, but the listed order minimises diff size per PR (state goes first, then queue, then assignments, then escalations, then PR mappings, then the lifecycle map, then RBAC).

## 5. Test strategy

### Unit tests

- `pkg/reconciler/reconciler_test.go`: extend `TestDeriveTicketState` to cover every new edge case introduced by step 5 (PR-merged dedup, fix-applied waiting, etc.). Already comprehensive — verify no regression.
- `pkg/lifecycle/manager_test.go`: add tests for `ListActiveAgents`, `ActiveJobCount`, `ReapFinishedJobs`, `HasJobForTicket` against a fake clientset. Some exist; cover error paths (API timeouts, empty namespace).
- `pkg/identity/manager_test.go`: add a `TestHydrateFromGitea` proving `NewManager` populates `agents` and `counts` from a fake Gitea client listing `general-agent-1`, `frontend-agent-3`. Replaces the `TestRegisterExisting` test deleted in step 3.
- `pkg/webhooks/gitea_handler_test.go`: extend `TestParseTicketIDFromPRBody` to cover the canonical agent PR body shapes.

### Integration tests

- New test file `cmd/orchestrator/restart_test.go` (or scripted as a verify step). Setup: spin up a fake K8s + Taiga + Gitea (via httptest), seed Taiga with one ticket in `ready` status and `[analysis:proceed]` already in comments, no Job in K8s. Run one `ReconcileAll`. Assert exactly one Job created with `mode=plan` for the right agent. Then **construct a fresh orchestrator from scratch** (simulating restart), run `ReconcileAll` again. Assert: no second Job is created (HasJobForTicket guards), agent identity is recovered via `AdoptExisting`. This is the canonical "survives restart with no state to lose" test.
- Add a similar fixture for the ticket #8 scenario: Taiga has `[analysis:proceed]` but no analysis Job in K8s (already terminated). Run `ReconcileAll`; assert plan agent spawns. This regression-locks the bug.

### End-to-end / manual

- Run through the full ticket lifecycle (analysis → plan → 3 steps → ready-for-test) in the live cluster, with no `orchestrator-state` ConfigMap present.
- Mid-flight pod restart: `kubectl rollout restart -n agents deployment/orchestrator`. Watch the new pod's first reconcile pass log every ticket; verify it lines up with what was in flight before the restart.
- Webhook redelivery: manually send a duplicate `pull_request_closed` (Gitea has a "redeliver" button); confirm only one escalation comment appears (Taiga-comment-based dedup at `main.go:582-590` does the work).
- Confirm `kubectl get configmap -n agents orchestrator-state` returns NotFound after step 3 deploys.

## 6. Open questions / decisions

1. **PR title/body convention.** `webhooks.ParseTicketIDFromPRBody` looks for `Ticket #N` and `ticket-N/`. After step 7 this is the only resolver. We need to confirm every agent path (analysis, plan, step, fix, onestep) embeds at least one of those in every PR it opens. The branch name itself is `ticket-{id}/...`, so the second pattern is structural — but we still need to verify the matcher handles `ticket-{id}/plan` correctly (the `/` after the digits is a stop boundary). `extractAfter` (`pkg/webhooks/gitea_handler.go:236-242`) does case-insensitive prefix match; `Sscanf("%d", ...)` on `12/plan` reads `12` and stops at `/`. Confirmed correct. **Action:** add a fuzz/unit test covering `Ticket #12`, `ticket-12/plan`, `ticket-12/step-3`, `Closes ticket-12/work`. No code change otherwise.

2. **Escalation count derivation.** The `escalations` map tracks "no-op reassignment" cycles. After step 6, this counter does not exist. **Question for the user:** is there any plan to revive the escalation feature? If yes, the derivation is "count of Gitea PRs on the ticket where `state=closed && !merged`" or "count of Taiga comments containing `[pr-rejected:N]`". If no, drop entirely. Recommend dropping; the PR-rejected handler at `main.go:567-620` already escalates by reassigning the ticket to the human on the *first* rejection.

3. **`onestep` mode handling in `pickAgentForTicketWithBusy`.** The function defers when the previous agent is busy (returns `errPreviousAgentBusy`). After step 1 the legacy `processQueue` re-enqueue path is gone; the reconciler simply moves on and tries again on the next pass. **Question:** is one-pass deferral acceptable latency for sticky-agent picking, given a 30 s reconcile interval? In the worst case a ticket waits one reconcile interval before retrying. If lower latency is needed, add an in-pass retry inside `ReconcileAll` (still single-pass, still no cross-pass state). Recommend leaving as-is and observing.

4. **`registeredRepos` cache.** It's per-process, not persisted, but it does drift if a human deletes the webhook in Gitea while the orchestrator is running. The orchestrator would silently stop registering it. **Question for the user:** keep as a same-process best-effort cache (renamed `seenWebhookRepos`, documented), or eliminate by listing repo webhooks before each `CreateRepoWebhook` call? Listing costs one extra Gitea call per unique-repo-encounter — negligible. Recommend eliminating: add `giteaClient.ListRepoWebhooks(owner, repo)` (one new helper, ~30 lines of HTTP), check for an existing hook by URL match, skip if present. Keeps the rule pristine.

5. **Identity-manager hydration cost.** Step 3's `HydrateFromGitea` lists all Gitea users matching `*-agent-*` at process startup. With N agents this is one paginated API call. For N < 100 this is fine. **Question:** is there a known cap on agent count? If not, paginate.

6. **Authoritative-mode rollout cadence.** The plan ships step 1 only after a week of shadow-mode validation. **Question for the user:** are we in shadow now? If shadow logs already show identical decisions to legacy logs for an extended period, step 1 can ship immediately. Otherwise, schedule the cutover one week out.
