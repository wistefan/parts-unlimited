This project should create an environment and orchestration setup for software development by Claude AI.

# The Input

Input to the agents is provided through a ticket system (Taiga). There is a single Taiga project for all agent-managed work. The user creates tickets that are used as input. Tickets require a title and description; agents decide independently whether the description (and any comments) contain sufficient information to begin work.

Agents are notified about new tickets in state "ready" or new input to those tickets via webhooks. Taiga retries failed webhook deliveries; additionally, agents poll for missed events on startup. Whether the webhook listener is a dedicated service or part of the orchestrating agent should be proposed in the implementation plan.

Agents check the tickets, analyze them, and request missing information through markdown-formatted comments (no prefix needed — agents comment under their own identity). Once a ticket contains enough information (agent's judgment), the agents start to work independently and the ticket is moved to "in progress".

Tickets are assigned to agents in FIFO order. The maximum number of agents working on tickets in parallel is configurable. The ticket lifecycle uses three states relevant to agents: **ready**, **in progress**, and **ready for test**. All other states belong to the human user and should be ignored by agents.

# Git Workflow

All work happens in a local Gitea instance. The ticket description must specify the target repository; if it does not, the agent must ask for it via a comment on the ticket.

For new projects, a new repo is created. For existing projects, the repo is checked out. Every work item is done on a dedicated branch with a PR back to the topic branch. Each agent always works on its own branch. Merge conflicts are resolved by the agent that created the PR; if it cannot resolve them, it requests human help via the ticket.

A single ticket can span work across multiple repositories (e.g., a backend API change and a corresponding frontend update). In that case, the implementation plan is created in the "main" repo — the one where most of the work will happen (determined by the agent). The plan outlines steps in other repos as needed. Every PR (including those in secondary repos) contains a link to the ticket and a reference to the relevant step in the plan. If merge ordering between repos matters, the agent either makes this clear in the PR description or creates the PRs sequentially.

The starting point for each ticket is an **Implementation Plan**, submitted as the first PR. The plan is a markdown document with sections and clear steps. Each step represents a feature/function-level increment that can be used on its own; concrete size is proposed by the agent. Steps are sequentially ordered, with parallelizable steps explicitly outlined. If the plan needs revision mid-implementation, the agent creates a new PR with the proposed changes.

The implementation plan PR must be approved by a human before any implementation work begins. Specialized agents may be asked to review and comment on the plan PR, but they cannot approve or merge it.

Each subsequent step of the plan is implemented as an individual PR. When the plan outlines parallelizable steps, the orchestrator spawns multiple agents to work on them simultaneously (if agents of the required specialization are available). The controlling agent reviews each PR and may request changes, but only a human can approve and merge. If a PR is fully rejected, the agent comments on the ticket and only resumes work when new instructions are given there.

All work must include sufficient tests to prevent breaking existing functionality through merges.

Branches are automatically deleted after merge.

When the agent opens the last PR of the implementation plan, the ticket transitions to **ready for test**. If change requests come in on that final PR, they are resolved. Once that PR is merged, the work is finished for the agents. At the transition to "ready for test", the agent adds a comment with release notes to the ticket.

# Work Environment

All work happens inside isolated, ephemeral containers — one container per agent. The container runtime should be fully runnable on a local environment; a local Kubernetes setup (e.g., k3s) is preferred to ease future scaling.

Agents configure the tools they need inside their containers. The typical tool set should be documented in the project for later reuse.

Agents can directly access the internet for read-only access to public services (package registries, documentation, etc.). If feasible, a mirror/proxy solution for package registries should be added to the setup. Agents interact with local services (Gitea, Taiga) under their own identity.

State is transitioned through tickets, the git repository, and potentially a state descriptor file (format to be proposed in the implementation plan). Containers themselves are ephemeral. The whole system must survive full restarts — recovery mechanisms and in-flight work resumption should be proposed in the implementation plan.

Permission/tool-usage policies are human-controlled and their format should be proposed as part of the implementation plan.

Idle agent container lifecycle (destroy immediately vs. keep alive with a timeout) is an implementation decision — a resource-effective approach should be proposed in the implementation plan.

# Agents

A controlling agent orchestrates worker agents to process tickets in parallel. It runs as a supervised process (e.g., Kubernetes deployment or systemd). Whether the orchestrator itself is a Claude AI instance or a traditional service that invokes Claude instances as workers should be proposed in the implementation plan.

The orchestrator is the only component with admin-level access to Gitea and Taiga. Since both services run locally, credential rotation is not required at this stage (potentially in future iterations).

There are two types of worker agents:
- **General-purpose agents** — handle the main implementation work on tickets.
- **Specialized agents** — handle specialized tasks. General-purpose agents delegate to specialized agents by reassigning the ticket to the appropriate Taiga role. The first available agent of that specialization picks up the ticket and reassigns it to its own identity. The requesting agent uses comments to provide instructions to the specialized agent.

Tickets can be multi-assigned to multiple specialized roles in parallel. For example, a general-purpose agent can assign a ticket to both the Frontend and Test roles simultaneously. Agents of both roles pick up the ticket and assign it to themselves, so the ticket is assigned to multiple agents concurrently. Each agent works on its own branch. When each specialized agent finishes, it reassigns the ticket back to the general-purpose agent. The general-purpose agent picks up the ticket once no specialized agents or roles remain assigned — it checks the assignment list before resuming. Whether this check is done by the general-purpose agent or managed by the orchestrator should be proposed in the implementation plan.

When a specialized agent finishes, one of two things happens:
- If the specialized agent completes the full ticket, it transitions the ticket to **ready for test**.
- If the ticket needs further work by the general-purpose agent, the specialized agent reassigns it back to that agent and comments with results.

If a specialized agent determines the delegated work is not needed, it reassigns the ticket back immediately with a comment explaining why. Escalation to the human is triggered after the second reassignment without actual work being done (i.e., two no-op reassignment cycles).

The starting set of specializations (Taiga roles) is: **Frontend**, **Backend**, **Test**, **Documentation**, **Operations**. If additional specializations are required by project needs, the orchestrator defines new roles dynamically.

Agent identities are created automatically by the orchestrating agent, on demand, in both Gitea and Taiga. Agent names include their specialization; the exact naming format should be proposed in the implementation plan. Each agent has its own identity, consistent across Gitea and Taiga, for traceability and correlation.

All permanent errors are escalated to the user via ticket comments. Retry behavior and timeouts are configurable. The human notification mechanism (for escalations, quota warnings, and tickets needing input) should be proposed in the implementation plan, taking into account that the system must work fully locally.

Context preservation across interactions (e.g., between creating a PR and responding to review feedback) should be proposed as part of the implementation plan. Agent failure mid-ticket recovery should also be proposed in the implementation plan.

# Testing and Quality

Agents run tests and linters according to best practices of the technology being used before creating a PR. Testing happens inside the agent's container unless specified otherwise.

CI/CD integration is done via Gitea Actions (GitHub Actions compatible). Agents run tests themselves; Gitea Actions runners are not required immediately but if added should execute in the same environment as the agents, not inside agent containers. Standard CI/CD workflows (test, pre-release, release) are mandatory for all repos and do not need to be outlined in the implementation plan. Only special CI/CD requirements need to be part of the plan.

# Security and Credentials

Git history and ticket history serve as the audit trail; no additional audit logging is needed.

Secret management and least-privilege permission models should be proposed as part of the implementation plan.

# Resource Management

No budget constraints or compute resource limits are enforced. The orchestrating agent should notify the user when a configured quota is reached. Cost tracking is not required at this stage.

# Open Questions

No remaining open questions. All design decisions have been resolved or explicitly deferred to the implementation plan.

## Items Deferred to the Implementation Plan

The following decisions have been explicitly deferred to the implementation plan. They are listed here for completeness so the plan addresses all of them:

1. Webhook listener architecture (dedicated service vs. part of orchestrator)
2. Permission/tool-usage policy format and storage
3. Agent naming convention format
4. Orchestrator nature (AI instance vs. traditional service)
5. Orchestrator recovery mechanism after restart
6. State descriptor file format and storage location
7. In-flight work resumption after system restart
8. Context preservation across agent interactions
9. Secret management approach
10. Least-privilege permission model
11. Human notification mechanism (must work fully locally)
12. Idle agent container lifecycle (resource-effective approach)
13. Agent failure mid-ticket recovery
14. Partial completion coordination (general-purpose agent check vs. orchestrator-managed)
