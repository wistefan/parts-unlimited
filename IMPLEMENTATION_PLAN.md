# Implementation Plan

## Token Usage Improvements

### Problem

Token usage statistics are currently collected per agent job in `agent/bootstrap.sh` (lines 721-774), aggregated across all turns and sessions, and appended as a markdown table to Taiga ticket comments. This has several limitations:

1. **No queryability** -- Metrics are embedded in free-text Taiga comments; there is no way to filter, aggregate, or trend token usage across tickets, modes, or time periods.
2. **Aggregate-only granularity** -- Only totals for input, output, cache write, and cache read tokens are recorded. Per-turn and per-session breakdowns are lost when sessions are aggregated (line 687: `cat "${SESSION_RESULT}" >> "${CUMULATIVE_RESULT}"`).
3. **No tool-use attribution** -- The stream-json output contains `tool_use` content blocks (tool name + input) alongside `text` blocks, but the current parser only reads `usage` fields and ignores tool invocations entirely (lines 742-743). There is no way to correlate token cost with specific tool calls.
4. **Blind spot: PR reviews** -- The orchestrator's review service (`orchestrator/pkg/review/service.go:110-115`) invokes Claude with `--output-format json`, which does not expose per-turn usage. Review token costs are invisible.
5. **No cache efficiency tracking** -- Cache hit ratio (cache\_read / total\_input per turn) is not computed. It's impossible to tell whether the system prompt caching strategy is effective or whether session chaining actually reduces cost.

### Goal

Ship a metrics pipeline that enables:

- Identifying the most expensive tickets, modes, steps, and tool calls.
- Tracking cache hit ratio per turn to validate that prompt caching and session chaining strategies are paying off.
- Spotting regressions (e.g., a new system prompt that defeats caching).
- Querying historical cost data across arbitrary time ranges and label dimensions.

### Proposed Architecture

```
agent pod (bootstrap.sh)          orchestrator (/metrics)
        |                                  |
        | push on completion               | scrape
        v                                  v
   Pushgateway  <----scrape----  Prometheus  ---> Grafana
                                     |
                                  alerting
                                 (optional)
```

**Prometheus + Pushgateway** -- Prometheus is the Kubernetes-native standard. Agents are ephemeral Jobs that cannot be scraped; the Pushgateway bridges this gap. The orchestrator is long-running and exposes a standard `/metrics` endpoint for direct scraping.

**Grafana** -- Dashboards for visualization and ad-hoc PromQL queries.

All three components deploy inside k3s alongside the existing services.

### Metrics Schema

#### Agent Metrics (pushed from `bootstrap.sh`)

These are pushed to Pushgateway once per agent job, after all sessions complete and before the pod terminates. Each metric carries labels for multi-dimensional querying.

**Common labels on all agent metrics:**

| Label | Source | Example |
|---|---|---|
| `ticket_id` | `$TICKET_ID` env var | `42` |
| `agent_id` | `$AGENT_ID` env var | `general-agent-1` |
| `mode` | `$MODE` env var | `analysis`, `plan`, `step`, `fix` |
| `specialization` | `$AGENT_SPECIALIZATION` env var | `general`, `frontend` |
| `plan_step` | `$PLAN_STEP` env var | `3` (empty for non-step modes) |
| `repo` | `$REPO_NAME` env var | `my-service` |

**Job-level metrics (one push per agent job):**

| Metric | Type | Description |
|---|---|---|
| `agent_tokens_input_total` | gauge | Total non-cached input tokens across all sessions |
| `agent_tokens_output_total` | gauge | Total output tokens |
| `agent_tokens_cache_write_total` | gauge | Total cache creation tokens |
| `agent_tokens_cache_read_total` | gauge | Total cache read tokens |
| `agent_turns_total` | gauge | Total turns across all sessions |
| `agent_sessions_total` | gauge | Number of sessions used |
| `agent_duration_seconds` | gauge | Wall-clock time from Claude invocation start to finish |

**Per-session metrics (one push per session, additional label `session`):**

| Metric | Type | Description |
|---|---|---|
| `agent_session_tokens_input` | gauge | Non-cached input tokens for this session |
| `agent_session_tokens_output` | gauge | Output tokens for this session |
| `agent_session_tokens_cache_write` | gauge | Cache creation tokens for this session |
| `agent_session_tokens_cache_read` | gauge | Cache read tokens for this session |
| `agent_session_turns` | gauge | Turns in this session |
| `agent_session_cache_hit_ratio` | gauge | `cache_read / (cache_read + input)` -- 0.0 to 1.0 |
| `agent_session_prompt_bytes` | gauge | Size of the session prompt in bytes |

**Per-turn metrics (one push per turn, additional labels `session` + `turn`):**

| Metric | Type | Description |
|---|---|---|
| `agent_turn_tokens_input` | gauge | Non-cached input tokens for this turn |
| `agent_turn_tokens_output` | gauge | Output tokens |
| `agent_turn_tokens_cache_write` | gauge | Cache creation tokens |
| `agent_turn_tokens_cache_read` | gauge | Cache read tokens |
| `agent_turn_cache_hit_ratio` | gauge | Per-turn cache hit ratio |

> **Cardinality note:** Per-turn metrics produce ~50 series per session x ~20 sessions x ~3 concurrent agents. At ~3,000 series per job this is well within Prometheus's comfort zone for a single-node deployment. If cardinality becomes a concern, per-turn metrics can be downsampled to per-session histograms.

**Tool-use metrics (one push per tool call, additional labels `session`, `turn`, `tool`):**

| Metric | Type | Description |
|---|---|---|
| `agent_tool_calls_total` | gauge | Count of calls to a given tool in this job |
| `agent_tool_tokens_after` | gauge | Cumulative input+cache_read tokens at the turn following the tool call (proxy for tool result size) |

The `tool` label values come from parsing `tool_use` content blocks in the stream-json output. Expected values: `Read`, `Edit`, `Write`, `Glob`, `Grep`, `Bash`, `Agent`, etc.

#### Orchestrator Metrics (scraped via `/metrics`)

The orchestrator is a long-running Go process. Use the standard `prometheus/client_golang` library to expose metrics on the existing HTTP server (port 8080).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `orchestrator_jobs_created_total` | counter | `mode`, `specialization` | Jobs created |
| `orchestrator_jobs_completed_total` | counter | `mode`, `specialization`, `status` | Jobs finished (`succeeded`/`failed`/`timeout`) |
| `orchestrator_job_duration_seconds` | histogram | `mode`, `specialization` | Time from job creation to completion |
| `orchestrator_review_invocations_total` | counter | `repo` | PR reviews performed |
| `orchestrator_review_tokens_input` | gauge | `repo`, `pr_number` | Input tokens per review (requires switching review CLI to `stream-json`) |
| `orchestrator_review_tokens_output` | gauge | `repo`, `pr_number` | Output tokens per review |
| `orchestrator_queue_depth` | gauge | | Number of tickets waiting for an agent |

### Implementation Steps

#### Step 1: Deploy Prometheus, Pushgateway, and Grafana

Create `k8s/monitoring/` with manifests for:

- **Prometheus** (`prom/prometheus:latest`) -- Deployment + ConfigMap for `prometheus.yml` with scrape targets for the orchestrator and Pushgateway. Service on ClusterIP port 9090.
- **Pushgateway** (`prom/pushgateway:latest`) -- Deployment + Service on ClusterIP port 9091, plus NodePort or hostPort so agent pods can reach it at a known address. Add the Pushgateway URL to the `agent-service-endpoints` ConfigMap.
- **Grafana** (`grafana/grafana:latest`) -- Deployment + Service on NodePort (e.g., `localhost:3000`). Provision the Prometheus data source via a ConfigMap-mounted provisioning file.
- Persistent storage via hostPath volumes at `/var/lib/dev-env/prometheus` and `/var/lib/dev-env/grafana`.

Update `k8s/namespaces.yaml` to add a `monitoring` namespace. Update `scripts/setup.sh` to deploy monitoring before the orchestrator. Update `scripts/teardown.sh` to clean up.

Update `k8s/agents/network-policy.yaml` to allow agent pods to reach the Pushgateway endpoint (same pattern as the existing Gitea/Taiga rules).

**Files:** `k8s/monitoring/prometheus.yaml`, `k8s/monitoring/pushgateway.yaml`, `k8s/monitoring/grafana.yaml`, `k8s/namespaces.yaml`, `scripts/setup.sh`, `scripts/teardown.sh`, `k8s/agents/network-policy.yaml`

#### Step 2: Extend the bootstrap.sh token parser

Replace the single-pass Python aggregation (lines 721-774) with a richer parser that:

1. **Iterates per-turn:** Extracts `usage` fields for each `type: "assistant"` message individually, tracking session and turn number.
2. **Parses tool\_use blocks:** For each `message.content` block with `type: "tool_use"`, records `block.name` as the tool name. Associates it with the current turn's token usage.
3. **Computes derived metrics:** Cache hit ratio per turn (`cache_read / (cache_read + input_tokens)`), per-session aggregates, and job-level totals.
4. **Pushes to Pushgateway:** Uses `curl` to POST metrics in Prometheus exposition format to `${PUSHGATEWAY_URL}/metrics/job/agent-worker/ticket_id/${TICKET_ID}/agent_id/${AGENT_ID}/mode/${MODE}`. The Pushgateway URL comes from the `agent-service-endpoints` ConfigMap (new key: `PUSHGATEWAY_URL`).
5. **Retains Taiga comment:** The existing markdown table is still generated and appended to the ticket comment for human readability, but now it's a summary derived from the same parsed data rather than the only record.

The parser should be extracted into a standalone Python script (`agent/parse-usage.py`) rather than an inline heredoc. This makes it testable and easier to maintain.

**Integration with session chaining:** Currently, usage is parsed only once from the cumulative `${RESULT_FILE}` after all sessions complete (line 724). The new parser should additionally run at the end of each session (inside the `while` loop at line 657) to emit per-session metrics immediately. This ensures metrics are captured even if the agent pod is killed before all sessions finish.

**Files:** `agent/parse-usage.py` (new), `agent/bootstrap.sh`, `agent/test-parse-usage.sh` (new, unit tests for the parser)

#### Step 3: Add orchestrator Prometheus metrics

Add `prometheus/client_golang` as a dependency. Register metrics in the orchestrator's HTTP handler (currently serves `/healthz` on port 8080). Expose a `/metrics` endpoint on the same port.

Instrument:

- `pkg/lifecycle/manager.go`: Increment `jobs_created_total` in `CreateJob`, observe `job_duration_seconds` and increment `jobs_completed_total` in `GetJobStatus` when a terminal state is detected.
- `pkg/assignment/engine.go` (or wherever the queue lives): Export `queue_depth` as a gauge reflecting current queue length.
- `pkg/review/service.go`: Switch from `--output-format json` to `--output-format stream-json`. Parse the stream-json output for both the review result (existing) and token usage (new). Record `review_tokens_*` metrics.

**Files:** `orchestrator/go.mod` (add prometheus client), `orchestrator/cmd/orchestrator/main.go`, `orchestrator/pkg/lifecycle/manager.go`, `orchestrator/pkg/review/service.go`, `orchestrator/pkg/metrics/metrics.go` (new -- metric registration)

#### Step 4: Provision Grafana dashboards

Create provisioned JSON dashboard files mounted into Grafana via ConfigMap. Three dashboards:

**Dashboard 1: Token Usage Overview**
- Total tokens over time (stacked by mode: analysis, plan, step, fix), split by token type (input, output, cache write, cache read).
- Tokens per ticket (table, sorted by total tokens descending).
- Tokens per step within a ticket (bar chart -- identifies expensive steps).
- Daily/weekly token trend line.

Key queries:
```promql
sum by (mode) (agent_tokens_input_total + agent_tokens_cache_write_total + agent_tokens_cache_read_total)
topk(10, sum by (ticket_id) (agent_tokens_output_total))
```

**Dashboard 2: Cache Efficiency**
- Cache hit ratio per turn (line chart, grouped by session) -- validates that prompt caching kicks in after turn 1.
- Cache hit ratio by mode (bar chart) -- compares analysis vs. plan vs. step vs. fix.
- Session-over-session cache write trend -- shows whether session chaining effectively limits cumulative cache writes.
- Cache read tokens as % of total input tokens (single stat panel).
- Turn-by-turn token breakdown for a selected ticket+session (drill-down).

Key queries:
```promql
avg by (mode) (agent_session_cache_hit_ratio)
agent_turn_cache_hit_ratio{ticket_id="$ticket", session="$session"}
```

**Dashboard 3: Tool Usage & Attribution**
- Tool call frequency by tool name (pie chart).
- Token cost following tool calls, by tool name (stacked bar -- proxy for "which tools produce expensive results?").
- Most-called tools per mode (heatmap).
- Tickets with unusually low cache hit ratio (table -- flags tickets where caching strategy is failing, indicating opportunities for prompt optimization).

Key queries:
```promql
sum by (tool) (agent_tool_calls_total)
topk(5, sum by (ticket_id) (agent_tool_calls_total{tool="Bash"}))
```

**Files:** `k8s/monitoring/grafana-dashboards.yaml` (ConfigMap), `k8s/monitoring/grafana.yaml` (mount the ConfigMap)

#### Step 5: Update documentation and configuration

- Update `CLAUDE.md` with: new monitoring URLs (`Prometheus: localhost:9090`, `Grafana: localhost:3000`), updated `import-images.sh` if monitoring images need caching, new env vars (`PUSHGATEWAY_URL`).
- Update `config.yaml` with a new `monitoring` section (Pushgateway URL, enable/disable flag).
- Update agent system prompts if needed (no changes expected -- agents don't interact with monitoring).
- Add `PUSHGATEWAY_URL` to `agent-service-endpoints` ConfigMap in `scripts/setup.sh`.

**Files:** `CLAUDE.md`, `config.yaml`, `scripts/setup.sh`, `k8s/agents/orchestrator.yaml` (ConfigMap)

### Identified Optimization Opportunities (enabled by this work)

Once the metrics pipeline is running, the following analyses become possible:

| Question | How to Answer | Potential Optimization |
|---|---|---|
| Which mode uses the most tokens per ticket? | `sum by (mode) (agent_tokens_input_total + agent_tokens_output_total + agent_tokens_cache_write_total)` | If `step` dominates, invest in better CLAUDE.md context to reduce tool-call exploration. |
| Does cache hit ratio drop in later sessions? | `agent_session_cache_hit_ratio` over session number | If yes, the session context summary may be too large or too different from the system prompt, defeating caching. Tune summary size. |
| Which tools cause the largest token spikes? | `agent_tool_tokens_after` by `tool` | If `Read` on large files dominates, add file size limits or pre-filter content before passing to the agent. |
| Are PR reviews token-heavy relative to their value? | `orchestrator_review_tokens_input` + `_output` vs. `review_invocations_total` | If reviews consume many tokens but rarely produce actionable issues, reduce review frequency or use a cheaper model. |
| Do some specializations use more tokens than others? | `sum by (specialization) (agent_tokens_input_total + agent_tokens_output_total)` | Route expensive specializations to cheaper models, or improve their CLAUDE.md context. |
| Is the 50-turn session limit optimal? | Compare `agent_session_cache_hit_ratio` and `agent_session_tokens_cache_write` across sessions | Tuning `TURNS_PER_SESSION` could reduce cache write costs. |
| How much does CLAUDE.md size affect first-turn cache writes? | Correlate `agent_session_prompt_bytes{session="1"}` with `agent_session_tokens_cache_write{session="1"}` | Right-size CLAUDE.md: too large = expensive cache writes; too small = more tool exploration later. |

### Rollout Plan

1. **Step 1** first (infrastructure). Verify Prometheus, Pushgateway, and Grafana are accessible.
2. **Steps 2 + 3** in parallel (agent metrics + orchestrator metrics). The agent and orchestrator are independent codebases.
3. **Step 4** after Steps 2-3 (dashboards require metrics to be flowing).
4. **Step 5** last (documentation reflects the final state).

Each step can be delivered as a separate PR to the topic branch.
