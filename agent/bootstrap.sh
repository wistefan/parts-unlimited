#!/usr/bin/env bash
# Agent worker bootstrap script.
# Reads environment variables, fetches ticket context, clones the repo,
# invokes Claude Code to perform the work, and creates/updates a PR on completion.
#
# Required environment variables:
#   TICKET_ID              - Taiga user story ID
#   AGENT_ID               - Agent identity name (e.g., general-agent-1)
#   AGENT_SPECIALIZATION   - Agent specialization (e.g., general, frontend, test)
#   GITEA_URL              - Gitea base URL
#   GITEA_USERNAME         - Gitea username for this agent
#   GITEA_PASSWORD         - Gitea password for this agent
#   TAIGA_URL              - Taiga base URL
#   TAIGA_USERNAME         - Taiga username for this agent
#   TAIGA_PASSWORD         - Taiga password for this agent
#
# Claude credentials are provided via a mounted file at
# /home/agent/.claude/.credentials.json (from K8s Secret).
#
# Optional environment variables:
#   MODE                   - Agent mode: analysis, plan, step, fix (default: step)
#   PLAN_STEP              - Implementation plan step number to work on
#   REPO_OWNER             - Gitea repo owner (extracted from ticket if not set)
#   REPO_NAME              - Gitea repo name (extracted from ticket if not set)
#   ALLOWED_TOOLS          - Space-separated list of allowed Claude tools
#   HUMAN_USERNAME         - Taiga username of the human user (for reassignment)
#   HUMAN_TAIGA_ID         - Taiga user ID of the human user (for reassignment)
#   PR_NUMBER              - PR number to fix (fix mode only)
#   PR_REPO                - "{owner}/{repo}" of the PR (fix mode only)

set -euo pipefail

WORKSPACE="/home/agent/workspace"
PROMPT_FILE="/home/agent/task-prompt.md"
RESULT_FILE="/home/agent/result.json"

# --- Validation ---

REQUIRED_VARS=(
    TICKET_ID AGENT_ID AGENT_SPECIALIZATION
    GITEA_URL GITEA_USERNAME GITEA_PASSWORD
    TAIGA_URL TAIGA_USERNAME TAIGA_PASSWORD
)

CLAUDE_CREDENTIALS="/home/agent/.claude-secret/.credentials.json"

for var in "${REQUIRED_VARS[@]}"; do
    if [ -z "${!var:-}" ]; then
        echo "ERROR: Required environment variable ${var} is not set"
        exit 1
    fi
done

if [ -z "${ANTHROPIC_API_KEY:-}" ] && [ ! -f "${CLAUDE_CREDENTIALS}" ]; then
    echo "ERROR: No Claude credentials found."
    echo "  Provide either ANTHROPIC_API_KEY env var or mount credentials at ${CLAUDE_CREDENTIALS}."
    exit 1
fi

MODE="${MODE:-step}"
PLAN_STEP="${PLAN_STEP:-}"
ALLOWED_TOOLS="${ALLOWED_TOOLS:-Read Edit Write Glob Grep Bash Task}"
HUMAN_USERNAME="${HUMAN_USERNAME:-}"
HUMAN_TAIGA_ID="${HUMAN_TAIGA_ID:-}"
PR_NUMBER="${PR_NUMBER:-}"
PR_REPO="${PR_REPO:-}"

# --- Helper: request human input and exit cleanly ---

# Posts a comment on the ticket, reassigns to the human user, and exits 0.
# Usage: request_human_input <comment_text>
request_human_input() {
    local comment="$1"
    local version

    # Include the canonical repo so the orchestrator (which keeps no
    # local state) can pin the next agent on this ticket to the same
    # fork. Safe when REPO_OWNER/REPO_NAME aren't resolved yet (very
    # early failures) — the line is just omitted.
    if [ -n "${REPO_OWNER:-}" ] && [ -n "${REPO_NAME:-}" ]; then
        comment="${comment}\n\n**Repo:** \`${REPO_OWNER}/${REPO_NAME}\`"
    fi

    # Dedupe: if the most recent agent comment already carries the same
    # signal (same first line — e.g. another silent-failure notice with
    # the same branch/mode/exit), the orchestrator has already been told
    # and will already have reassigned to the human. Posting the same
    # text again would just spam the ticket. Exit silently so the Job
    # still completes cleanly and the orchestrator's reconcile loop
    # won't try to respawn a human-assigned ticket.
    # x-disable-pagination: Taiga's history endpoint paginates at 30
    # entries by default; on long-running tickets the most recent
    # agent comment can fall off the first page and dedupe would miss.
    local latest_agent_comment
    latest_agent_comment=$(curl -s -H "${TAIGA_AUTH_HEADER}" -H "x-disable-pagination: True" \
        "${TAIGA_URL}/api/v1/history/userstory/${TICKET_ID}" 2>/dev/null \
        | jq -r '[.[] | select(.comment != null and .comment != "" and .delete_comment_date == null and (.user.username // "" | contains("-agent-")))] | sort_by(.created_at) | last | .comment // ""' 2>/dev/null || true)
    local first_line
    first_line=$(printf '%b' "${comment}" | head -n1)
    if [ -n "${first_line}" ] && [ -n "${latest_agent_comment}" ] \
            && printf '%s' "${latest_agent_comment}" | head -n1 | grep -qF "${first_line}"; then
        echo "  Same notice already posted by an agent — skipping duplicate comment."
        echo ""
        echo "=== Agent Worker Complete (awaiting human input, deduped) ==="
        exit 0
    fi

    # Fetch current ticket version
    version=$(curl -s -H "${TAIGA_AUTH_HEADER}" \
        "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
        | jq -r '.version') || true

    if [ -z "${version}" ] || [ "${version}" = "null" ]; then
        echo "WARNING: Could not fetch ticket version."
        echo ""
        echo "=== Agent Worker Complete (awaiting human input) ==="
        exit 0
    fi

    # Build the patch payload: always include comment, optionally reassign
    local patch_data
    if [ -n "${HUMAN_TAIGA_ID}" ]; then
        patch_data="{\"comment\": $(echo "${comment}" | jq -Rs .), \"assigned_to\": ${HUMAN_TAIGA_ID}, \"version\": ${version}}"
    else
        patch_data="{\"comment\": $(echo "${comment}" | jq -Rs .), \"version\": ${version}}"
    fi

    local response
    response=$(curl -s -w "\n%{http_code}" -X PATCH "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
        -H "${TAIGA_AUTH_HEADER}" \
        -H "Content-Type: application/json" \
        -d "${patch_data}") || true

    local http_code
    http_code=$(echo "${response}" | tail -1)
    if [ "${http_code}" = "200" ]; then
        echo "  Comment posted and ticket reassigned to ${HUMAN_USERNAME:-human}."
    else
        echo "WARNING: Ticket update failed (HTTP ${http_code})."
        echo "  Response: $(echo "${response}" | head -n -1)"
    fi

    echo ""
    echo "=== Agent Worker Complete (awaiting human input) ==="
    exit 0
}

echo "=== Agent Worker Bootstrap ==="
echo "  Agent:          ${AGENT_ID}"
echo "  Specialization: ${AGENT_SPECIALIZATION}"
echo "  Ticket:         ${TICKET_ID}"
echo "  Plan step:      ${PLAN_STEP:-none}"

# --- Configure git identity ---

git config --global user.name "${AGENT_ID}"
git config --global user.email "${AGENT_ID}@dev-env.local"
# The workspace emptyDir volume is created by kubelet (root-owned) but the
# container runs as UID 1000.  Mark it safe so git does not refuse to operate.
git config --global --add safe.directory "${WORKSPACE}"

# --- Authenticate with Taiga ---

echo "Authenticating with Taiga..."
TAIGA_AUTH=$(curl -sf -X POST "${TAIGA_URL}/api/v1/auth" \
    -H "Content-Type: application/json" \
    -d "{\"type\": \"normal\", \"username\": \"${TAIGA_USERNAME}\", \"password\": \"${TAIGA_PASSWORD}\"}")

TAIGA_TOKEN=$(echo "${TAIGA_AUTH}" | jq -r '.auth_token')
if [ -z "${TAIGA_TOKEN}" ] || [ "${TAIGA_TOKEN}" = "null" ]; then
    echo "ERROR: Failed to authenticate with Taiga"
    exit 1
fi
echo "  Taiga authenticated."

TAIGA_AUTH_HEADER="Authorization: Bearer ${TAIGA_TOKEN}"

# --- Fetch ticket ---

echo "Fetching ticket ${TICKET_ID}..."
TICKET=$(curl -sf -H "${TAIGA_AUTH_HEADER}" "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}")

TICKET_SUBJECT=$(echo "${TICKET}" | jq -r '.subject')
TICKET_DESCRIPTION=$(echo "${TICKET}" | jq -r '.description // ""')
TICKET_VERSION=$(echo "${TICKET}" | jq -r '.version')
TICKET_TAGS=$(echo "${TICKET}" | jq -r '[.tags[]?[0]] | join(", ")')

echo "  Subject: ${TICKET_SUBJECT}"
echo "  Tags:    ${TICKET_TAGS:-none}"

# --- Fetch ticket comments ---
#
# To reduce token usage, we only pass relevant context to the agent:
#   1. The last agent comment (which should contain a cumulative context summary)
#   2. Any human comments posted after it (new input the agent needs to see)
# This gives full context in a fraction of the tokens.

echo "Fetching ticket comments..."
# x-disable-pagination: Taiga paginates at 30 by default; without this
# the "last agent comment" and context-summary logic below would miss
# entries on long-running tickets.
ALL_COMMENTS=$(curl -sf -H "${TAIGA_AUTH_HEADER}" -H "x-disable-pagination: True" \
    "${TAIGA_URL}/api/v1/history/userstory/${TICKET_ID}" \
    | jq -r '[.[] | select(.comment != null and .comment != "" and .delete_comment_date == null) | {user: .user.username, comment: .comment, date: .created_at}]')
COMMENT_COUNT=$(echo "${ALL_COMMENTS}" | jq 'length')
echo "  Total comments: ${COMMENT_COUNT}"

# Find the index of the last agent comment (any user containing "-agent-").
# Comments are returned newest-first by Taiga; we sort oldest-first for indexing.
COMMENTS_ASC=$(echo "${ALL_COMMENTS}" | jq 'sort_by(.date)')
LAST_AGENT_IDX=$(echo "${COMMENTS_ASC}" | jq '[.[] | .user] | to_entries | map(select(.value | contains("-agent-"))) | last | .key // -1')

if [ "${LAST_AGENT_IDX}" -ge 0 ] 2>/dev/null; then
    # Extract: the last agent comment + all comments after it (human input)
    COMMENTS=$(echo "${COMMENTS_ASC}" | jq --argjson idx "${LAST_AGENT_IDX}" '.[$idx:]')
    RELEVANT_COUNT=$(echo "${COMMENTS}" | jq 'length')
    echo "  Relevant comments (last agent + newer): ${RELEVANT_COUNT}"
else
    # No agent comments yet — pass latest 5 for initial context
    COMMENTS=$(echo "${COMMENTS_ASC}" | jq '.[-5:]')
    echo "  No prior agent comments, using latest 5"
fi

# --- Determine repo from ticket or env ---

# Extracts the repo reference from a ticket description line.
# Handles plain owner/name, full URLs, and markdown-formatted links.
# Outputs the cleaned value (URL or owner/name) to stdout.
extract_repo_ref() {
    local desc="$1"
    # Grab everything after "repo:" or "gitea:" (case-insensitive)
    local raw
    raw=$(echo "${desc}" | grep -oP '(?i)(?:repo|gitea)\s*:\s*\K.*' | head -1 || true)
    [ -z "${raw}" ] && return

    # Strip leading/trailing whitespace AND invisible unicode codepoints
    # that sed `\s` does NOT handle but can easily arrive via rich-text
    # copy-paste into Taiga: U+00A0 NBSP, U+200B-U+200F (zero-width family),
    # U+FEFF BOM. Leaving these intact makes later anchors like `^https?://`
    # silently fail and the code routes to the owner/name branch with
    # garbled output (observed: REPO_OWNER=" https:" / REPO_NAME="").
    raw=$(printf '%s' "${raw}" | python3 -c '
import sys, unicodedata
s = sys.stdin.read()
def strippable(c):
    # Stdlib whitespace PLUS the Cf (format) unicode category, which is
    # where the zero-width / bidi-control / BOM codepoints live.
    return c.isspace() or unicodedata.category(c) == "Cf"
i, j = 0, len(s)
while i < j and strippable(s[i]):
    i += 1
while j > i and strippable(s[j - 1]):
    j -= 1
sys.stdout.write(s[i:j])
')

    # Strip markdown link syntax: [text](url) → url
    if echo "${raw}" | grep -qP '^\[.*\]\(.*\)'; then
        raw=$(echo "${raw}" | sed -E 's/^\[([^]]*)\]\(([^)]*)\).*/\2/')
    fi
    # Strip bare markdown brackets: [url] → url
    raw=$(echo "${raw}" | sed -E 's/^\[([^]]*)\]$/\1/')
    # Strip angle brackets: <url> → url
    raw=$(echo "${raw}" | sed 's/^<//;s/>$//')
    # Take only the first whitespace-delimited token (drop trailing comments etc.)
    raw=$(echo "${raw}" | awk '{print $1}')

    echo "${raw}"
}

if [ -n "${REPO_OWNER:-}" ] && [ -n "${REPO_NAME:-}" ]; then
    echo "Using repo from environment: ${REPO_OWNER}/${REPO_NAME}"
else
    REPO_REF=$(extract_repo_ref "${TICKET_DESCRIPTION}")

    if [ -z "${REPO_REF}" ]; then
        echo "Cannot determine target repository — requesting human input."
        request_human_input "Cannot determine the target repository. Please specify the repository in the description (e.g., \`repo: owner/name\`) or provide it in a comment."
    elif echo "${REPO_REF}" | grep -qP '^https?://'; then
        # Remote URL — import into local Gitea
        REMOTE_URL="${REPO_REF}"
        CLEAN_URL="${REMOTE_URL%.git}"
        CLEAN_URL="${CLEAN_URL%/}"
        REPO_NAME=$(basename "${CLEAN_URL}")
        REPO_OWNER="${GITEA_USERNAME}"

        echo "Detected remote repository: ${REMOTE_URL}"
        echo "Importing into Gitea as ${REPO_OWNER}/${REPO_NAME}..."

        if curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
            "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}" > /dev/null 2>&1; then
            echo "  Repo already exists in Gitea."
        else
            MIGRATE_RESPONSE=$(curl -s -w "\n%{http_code}" \
                -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
                -X POST "${GITEA_URL}/api/v1/repos/migrate" \
                -H "Content-Type: application/json" \
                -d "{
                    \"clone_addr\": \"${REMOTE_URL}\",
                    \"repo_name\": \"${REPO_NAME}\",
                    \"repo_owner\": \"${REPO_OWNER}\",
                    \"service\": \"git\",
                    \"mirror\": false
                }")
            MIGRATE_CODE=$(echo "${MIGRATE_RESPONSE}" | tail -1)
            if [ "${MIGRATE_CODE}" = "201" ] || [ "${MIGRATE_CODE}" = "200" ]; then
                echo "  Migration accepted — waiting for import to finish..."
            else
                echo "ERROR: Failed to import repo from ${REMOTE_URL} (HTTP ${MIGRATE_CODE})"
                echo "  Response: $(echo "${MIGRATE_RESPONSE}" | head -n -1)"
                request_human_input "Failed to import repository from \`${REMOTE_URL}\` into Gitea (HTTP ${MIGRATE_CODE}). Please create it manually or check the URL."
            fi
        fi

        # Gitea migrations can be async.  Poll until the repo is cloneable
        # (i.e. its default branch exists) or until we time out.
        MIGRATE_WAIT_MAX=60
        MIGRATE_WAIT=0
        MIGRATE_POLL_INTERVAL=3
        while [ "${MIGRATE_WAIT}" -lt "${MIGRATE_WAIT_MAX}" ]; do
            REPO_STATUS=$(curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
                "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}" 2>/dev/null || true)
            if [ -n "${REPO_STATUS}" ]; then
                REPO_EMPTY=$(echo "${REPO_STATUS}" | jq -r '.empty')
                if [ "${REPO_EMPTY}" = "false" ]; then
                    echo "  Repo ready."
                    break
                fi
            fi
            sleep "${MIGRATE_POLL_INTERVAL}"
            MIGRATE_WAIT=$((MIGRATE_WAIT + MIGRATE_POLL_INTERVAL))
            echo "  Waiting for migration to complete... (${MIGRATE_WAIT}s/${MIGRATE_WAIT_MAX}s)"
        done
        if [ "${MIGRATE_WAIT}" -ge "${MIGRATE_WAIT_MAX}" ]; then
            echo "ERROR: Migration did not complete within ${MIGRATE_WAIT_MAX}s."
            request_human_input "Repository migration from \`${REMOTE_URL}\` timed out. The repo may still be importing — check Gitea and retry."
        fi

        echo "Using imported repo: ${REPO_OWNER}/${REPO_NAME}"
    elif echo "${REPO_REF}" | grep -q '/'; then
        # Local owner/name format
        REPO_OWNER=$(echo "${REPO_REF}" | cut -d/ -f1)
        REPO_NAME=$(echo "${REPO_REF}" | cut -d/ -f2)
        echo "Extracted repo from description: ${REPO_OWNER}/${REPO_NAME}"
    else
        # Plain repo name — greenfield project.  Create under the agent's
        # own Gitea identity if it does not exist yet.
        REPO_NAME="${REPO_REF}"
        REPO_OWNER="${GITEA_USERNAME}"
        echo "Detected plain repo name: ${REPO_NAME}"

        if curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
            "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}" > /dev/null 2>&1; then
            echo "  Repo ${REPO_OWNER}/${REPO_NAME} already exists in Gitea."
        else
            echo "Creating new repo ${REPO_OWNER}/${REPO_NAME} in Gitea..."
            CREATE_RESPONSE=$(curl -s -w "\n%{http_code}" \
                -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
                -X POST "${GITEA_URL}/api/v1/user/repos" \
                -H "Content-Type: application/json" \
                -d "{
                    \"name\": \"${REPO_NAME}\",
                    \"auto_init\": true,
                    \"default_branch\": \"main\"
                }")
            CREATE_CODE=$(echo "${CREATE_RESPONSE}" | tail -1)
            if [ "${CREATE_CODE}" = "201" ]; then
                echo "  Created repo ${REPO_OWNER}/${REPO_NAME}"
            else
                echo "ERROR: Failed to create repo ${REPO_NAME} (HTTP ${CREATE_CODE})"
                echo "  Response: $(echo "${CREATE_RESPONSE}" | head -n -1)"
                request_human_input "Failed to create repository \`${REPO_NAME}\` in Gitea (HTTP ${CREATE_CODE}). Please create it manually."
            fi
        fi

        echo "Using repo: ${REPO_OWNER}/${REPO_NAME}"
    fi
fi

# --- Clone repo ---

echo "Cloning ${REPO_OWNER}/${REPO_NAME}..."
CLONE_URL="http://${GITEA_USERNAME}:${GITEA_PASSWORD}@${GITEA_URL#http://}/${REPO_OWNER}/${REPO_NAME}.git"
# Strip protocol for URL construction
GITEA_HOST="${GITEA_URL#http://}"
GITEA_HOST="${GITEA_HOST#https://}"
CLONE_URL="http://${GITEA_USERNAME}:${GITEA_PASSWORD}@${GITEA_HOST}/${REPO_OWNER}/${REPO_NAME}.git"

mkdir -p "${WORKSPACE}"
git clone "${CLONE_URL}" "${WORKSPACE}"
cd "${WORKSPACE}"

# --- Extract base branch from ticket (optional "base: <branch>" field) ---

extract_base_branch() {
    local desc="$1"
    local raw
    raw=$(echo "${desc}" | grep -oP '(?i)base\s*:\s*\K\S+' | head -1 || true)
    echo "${raw}"
}

# count_merged_step_prs returns the number of merged step PRs for this
# ticket on the current repo. PR records persist on Gitea even when
# branches are deleted on merge, so this is a stable source of truth for
# "how many steps have already shipped" — unlike `git ls-remote ... step-*`
# which silently regresses to zero when the repo has delete-branch-on-merge
# enabled.
#
# A merged step PR is identified by two stable signals that both survive
# branch deletion (Gitea rewrites a deleted head ref to `refs/pull/N/head`,
# so head-ref prefix matching alone undercounts):
#   - base.ref == "ticket-${TICKET_ID}/work"  (every step PR targets work)
#   - title  starts with  "Ticket #${TICKET_ID} - Step "
#                          (the format hard-coded by bootstrap.sh below)
#
# Requires REPO_OWNER, REPO_NAME, TICKET_ID, GITEA_URL, GITEA_USERNAME,
# GITEA_PASSWORD to be set.
PR_PAGE_SIZE=50
count_merged_step_prs() {
    local page=1 total=0 page_json page_count batch
    local title_prefix="Ticket #${TICKET_ID} - Step "
    local work_base="ticket-${TICKET_ID}/work"
    while :; do
        page_json=$(curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
            "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/pulls?state=closed&limit=${PR_PAGE_SIZE}&page=${page}" \
            2>/dev/null || echo '[]')
        page_count=$(echo "${page_json}" | jq 'length' 2>/dev/null || echo 0)
        batch=$(echo "${page_json}" \
            | jq --arg base "${work_base}" --arg tprefix "${title_prefix}" \
                "[.[] | select(.merged == true)
                       | select(.base.ref == \$base)
                       | select(.title | startswith(\$tprefix))
                  ] | length" 2>/dev/null || echo 0)
        total=$((total + batch))
        [ "${page_count}" -lt "${PR_PAGE_SIZE}" ] && break
        page=$((page + 1))
    done
    echo "${total}"
}

BASE_BRANCH=$(extract_base_branch "${TICKET_DESCRIPTION}")
BASE_BRANCH="${BASE_BRANCH:-main}"
echo "Base branch: ${BASE_BRANCH}"

# --- Refresh base branch from upstream (migrated repos only) ---
#
# When a repo is imported into Gitea from an external URL (e.g. GitHub),
# the Gitea copy is a point-in-time snapshot. Later tickets clone that
# stale copy, and PRs cut from its `main` conflict with the real upstream
# `main` when they eventually merge back. To prevent that, fetch the
# upstream ${BASE_BRANCH} and force-overwrite the Gitea copy before any
# work-branch bookkeeping runs. Existing work/step branches are not
# touched — they continue to live in Gitea only. Greenfield or Gitea-
# native repos have no `original_url` and are skipped.
#
# Full-overwrite semantics (reset --hard + push --force) are intentional:
# the Gitea copy is a working mirror of upstream, not a source of truth.
REPO_METADATA=$(curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
    "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}" 2>/dev/null || true)
UPSTREAM_URL=$(echo "${REPO_METADATA}" | jq -r '.original_url // ""' 2>/dev/null || true)

if [ -n "${UPSTREAM_URL}" ] && [ "${UPSTREAM_URL}" != "null" ]; then
    echo "Refreshing ${BASE_BRANCH} from upstream ${UPSTREAM_URL}..."
    git remote add upstream "${UPSTREAM_URL}" 2>/dev/null \
        || git remote set-url upstream "${UPSTREAM_URL}"
    if git fetch upstream "${BASE_BRANCH}" 2>/dev/null; then
        git checkout -B "${BASE_BRANCH}" "upstream/${BASE_BRANCH}"
        if git push origin "${BASE_BRANCH}" --force 2>/dev/null; then
            echo "  Gitea ${BASE_BRANCH} overwritten to upstream HEAD."
        else
            echo "  WARNING: Could not force-push refreshed ${BASE_BRANCH} to Gitea — continuing with local refresh only."
        fi
    else
        echo "  WARNING: Could not fetch ${BASE_BRANCH} from upstream ${UPSTREAM_URL} — using existing Gitea copy."
    fi
else
    echo "No upstream URL on Gitea repo — greenfield or Gitea-native, skipping refresh."
fi

# --- Determine branch ---

BRANCH_PREFIX="ticket-${TICKET_ID}"
case "${MODE}" in
    analysis)
        # Analysis mode may not need a branch (works on default branch).
        BRANCH_NAME=""
        ;;
    plan)
        BRANCH_NAME="${BRANCH_PREFIX}/plan"
        ;;
    onestep)
        # One-step tickets ship as a single PR against the work branch.
        # A dedicated `onestep` sub-branch keeps the naming distinct from
        # the step-N series (there is no step numbering here) and avoids
        # any confusion with chained step agents.
        BRANCH_NAME="${BRANCH_PREFIX}/onestep"
        ;;
    fix)
        # Fix mode: check out the existing PR branch (bootstrap determines it later
        # from the PR data).  Set a placeholder — overridden after PR fetch.
        BRANCH_NAME=""
        ;;
    step|*)
        # Step agents always work on a step branch so the resulting PR
        # targets the integration work branch (never `main`). Working
        # directly on the work branch would cause a work → BASE_BRANCH
        # PR, which this repo never wants.
        #
        # PLAN_STEP is the 1-based index of the step this agent is about
        # to ship. It must be derived from a source that survives
        # delete-branch-on-merge, otherwise the counter regresses once
        # earlier step branches are reaped (observed: ticket #22 going
        # `[step:3/6]` → `[step:1/6]`). Counting *merged* step PRs is
        # stable: PR records persist forever on Gitea.
        if [ -z "${PLAN_STEP}" ]; then
            MERGED_STEP_PR_COUNT=$(count_merged_step_prs)
            PLAN_STEP=$((MERGED_STEP_PR_COUNT + 1))
            echo "Derived PLAN_STEP=${PLAN_STEP} from ${MERGED_STEP_PR_COUNT} merged step PR(s)."
        fi
        BRANCH_NAME="${BRANCH_PREFIX}/step-${PLAN_STEP}"
        ;;
esac

# Ensure the work branch exists (all modes except analysis branch off it)
WORK_BRANCH="${BRANCH_PREFIX}/work"
if [ "${MODE}" != "analysis" ]; then
    if git ls-remote --heads origin "${WORK_BRANCH}" | grep -q "${WORK_BRANCH}"; then
        echo "Work branch exists: ${WORK_BRANCH}"
        git fetch origin "${WORK_BRANCH}"
    else
        echo "Creating work branch: ${WORK_BRANCH} from ${BASE_BRANCH}"
        git checkout -b "${WORK_BRANCH}" "origin/${BASE_BRANCH}"
        git push -u origin "${WORK_BRANCH}"
    fi
fi

# Check out or create the target branch
if [ -n "${BRANCH_NAME}" ]; then
    if git ls-remote --heads origin "${BRANCH_NAME}" | grep -q "${BRANCH_NAME}"; then
        echo "Resuming work on existing branch: ${BRANCH_NAME}"
        git checkout "${BRANCH_NAME}"
    else
        echo "Creating new branch: ${BRANCH_NAME} from ${WORK_BRANCH}"
        git checkout -b "${BRANCH_NAME}" "origin/${WORK_BRANCH}"
    fi
fi

# --- Read implementation plan if it exists ---

PLAN_CONTENT=""
for plan_file in IMPLEMENTATION_PLAN.md implementation_plan.md PLAN.md plan.md; do
    if [ -f "${plan_file}" ]; then
        PLAN_CONTENT=$(cat "${plan_file}")
        echo "Found implementation plan: ${plan_file}"
        break
    fi
done

# For step mode, embed ONLY the current step (plus the plan preamble) in
# the task prompt. The full plan lives in IMPLEMENTATION_PLAN.md in the
# workspace, which the agent can Read on demand — there is no reason to pay
# ~N × (other_steps_tokens) cache reads across every turn of session 1.
#
# Format contract: the plan uses `### Step N: Title` headings (see
# system-prompt-step.md). Preamble = everything before the first such
# heading; per-step bodies run from their heading until the next `### Step`
# heading or EOF.
if [ "${MODE}" = "step" ] && [ -n "${PLAN_STEP}" ] && [ -n "${PLAN_CONTENT}" ]; then
    PLAN_CONTENT=$(PLAN_STEP_NUM="${PLAN_STEP}" python3 -c '
import os, re, sys
raw = sys.stdin.read()
step = os.environ["PLAN_STEP_NUM"]
# Split keeping the step headings as delimiters. With a capturing group,
# re.split yields: [preamble, heading1, body1, heading2, body2, ...].
parts = re.split(r"(?m)^(### Step \d+[^\n]*)$", raw)
out = [parts[0].rstrip() + "\n"] if parts and parts[0].strip() else []
found = False
for i in range(1, len(parts), 2):
    heading = parts[i]
    body = parts[i + 1] if i + 1 < len(parts) else ""
    m = re.match(r"### Step (\d+)", heading)
    if m and m.group(1) == step:
        out.append(heading + body.rstrip() + "\n")
        found = True
        break
if not found:
    # Fall back to full plan if the step heading is missing — better than
    # feeding the agent an empty plan section.
    sys.stdout.write(raw)
else:
    sys.stdout.write("".join(out))
    sys.stdout.write(
        "\n_Note: only step " + step + " is shown; "
        "read IMPLEMENTATION_PLAN.md in the workspace root for prior/later "
        "steps if needed._\n"
    )
' <<< "${PLAN_CONTENT}")
    echo "  Sliced plan to step ${PLAN_STEP} only ($(printf '%s' "${PLAN_CONTENT}" | wc -c) bytes)"
fi

# --- Build task prompt ---

echo "Building task prompt..."

# Build the mode-specific instructions section
MODE_INSTRUCTIONS=""
case "${MODE}" in
    analysis)
        MODE_INSTRUCTIONS="You are an analysis agent. Evaluate this ticket and determine if it is clear enough to create an implementation plan.

Your ONLY task is to write /home/agent/completion-status.json with your analysis.
Do NOT write code, create branches, or make commits.

If the ticket is clear enough to proceed:
\`\`\`json
{
  \"status\": \"success\",
  \"summary\": \"Analysis complete: ticket is ready for implementation planning.\",
  \"analysis_result\": \"proceed\",
  \"analysis_comment\": \"[analysis:proceed]\\n\\n**Analysis Summary:**\\n<your understanding>\\n\\n**Repositories:** <repos>\\n**Base branch:** <branch>\\n**Estimated steps:** <number>\"
}
\`\`\`

If more information is needed:
\`\`\`json
{
  \"status\": \"blocked\",
  \"reason\": \"Additional information required: <summary>\",
  \"analysis_result\": \"need-info\",
  \"analysis_comment\": \"[analysis:need-info]\\n\\n**Missing Information:**\\n<specific questions>\"
}
\`\`\`"
        ;;
    plan)
        MODE_INSTRUCTIONS="You are a planning agent. Create an implementation plan for this ticket.

Write IMPLEMENTATION_PLAN.md in the repo root with steps using \`### Step N: Title\` format.
Commit the plan. Do not implement any code.

When done, write /home/agent/completion-status.json:
\`\`\`json
{
  \"status\": \"success\",
  \"summary\": \"Implementation plan created with N steps.\",
  \"taiga_comment\": \"[phase:plan-created]\\n\\n**Steps:** N\\n**Summary:** <one-line summary>\"
}
\`\`\`"
        ;;
    onestep)
        MODE_INSTRUCTIONS="You are a one-step implementation agent. The ticket is tagged \`one-step\` and the analysis agent agreed it fits in a single PR.

Implement the full ticket on the \`${BRANCH_PREFIX}/onestep\` branch (already checked out). No plan, no step splitting — one focused change.

Run tests and linters before finishing. If you discover the work does NOT fit in one PR, stop and hand back to the human (see completion schema below).

When done, write /home/agent/completion-status.json:

Happy path (ready for review):
\`\`\`json
{
  \"status\": \"success\",
  \"summary\": \"Implemented one-step ticket: <short title>\",
  \"taiga_comment\": \"[step:complete]\\n\\nOne-step implementation complete.\\n\\n**Summary:** <what was changed and why>\\n\\n**Release Notes:**\\n<human-readable summary of the changes>\"
}
\`\`\`

If the scope turned out too large:
\`\`\`json
{
  \"status\": \"blocked\",
  \"reason\": \"Scope exceeds one-step contract: <concrete reason>\",
  \"summary\": \"One-step implementation aborted — scope too large.\",
  \"taiga_comment\": \"[analysis:onestep-rejected]\\n\\n**Scope exceeded during implementation:**\\n<what you found, which files/subsystems are involved, why it needs splitting>.\\n\\n**Suggestion:** remove the \`one-step\` tag and re-run analysis for a multi-step plan, or split the ticket.\"
}
\`\`\`"
        ;;
    fix)
        MODE_INSTRUCTIONS="You are a fix agent. Address the review comments on the existing PR.

Review the PR diff and comments below. Fix every issue raised.
Commit your changes. Do not create new branches or PRs.

When done, write /home/agent/completion-status.json:
\`\`\`json
{
  \"status\": \"success\",
  \"summary\": \"Addressed N review comments on PR #${PR_NUMBER}.\",
  \"taiga_comment\": \"[fix:applied]\\n\\nAddressed review feedback on PR #${PR_NUMBER}: <summary>\"
}
\`\`\`"
        ;;
    step|*)
        MODE_INSTRUCTIONS="You are a step implementation agent. Implement the next step from the implementation plan.

Read IMPLEMENTATION_PLAN.md, determine which step to work on next, implement it.
Create a step branch, commit, and follow the plan.

When done, write /home/agent/completion-status.json:
If more steps remain:
\`\`\`json
{
  \"status\": \"success\",
  \"summary\": \"Implemented step N of M: <title>\",
  \"taiga_comment\": \"[step:N/M]\\n\\nCompleted step N of M: <title>\\n\\n**Summary:** <description>\"
}
\`\`\`
If this was the last step:
\`\`\`json
{
  \"status\": \"success\",
  \"summary\": \"All steps complete.\",
  \"taiga_comment\": \"[step:complete]\\n\\nAll steps completed.\\n\\n**Release Notes:**\\n<summary of all changes>\"
}
\`\`\`"
        ;;
esac

# The ticket description is load-bearing for analysis (evaluating the ask)
# and plan (drafting steps). For step and fix modes, IMPLEMENTATION_PLAN.md
# (or the PR diff) already encapsulates the work, so the description is
# redundant — and on chained sessions it gets re-ingested uncached every
# time, costing real tokens on long-running tickets. Omit it in those modes.
case "${MODE}" in
    analysis|plan|onestep)
        # Onestep has no IMPLEMENTATION_PLAN.md — the ticket description
        # itself is the spec, so we must keep it in the prompt.
        DESCRIPTION_SECTION="## Ticket Description

${TICKET_DESCRIPTION}
"
        ;;
    *)
        DESCRIPTION_SECTION=""
        ;;
esac

cat > "${PROMPT_FILE}" <<PROMPT_EOF
# Task Assignment

## Agent Identity
- **Agent:** ${AGENT_ID}
- **Specialization:** ${AGENT_SPECIALIZATION}
- **Mode:** ${MODE}

## Ticket
- **ID:** ${TICKET_ID}
- **Subject:** ${TICKET_SUBJECT}
- **Tags:** ${TICKET_TAGS:-none}
$([ -n "${PLAN_STEP}" ] && echo "- **Plan Step:** ${PLAN_STEP}" || true)

${DESCRIPTION_SECTION}
## Ticket Comments (last agent context + new human input)

$(echo "${COMMENTS}" | jq -r '.[] | "**\(.user)** (\(.date)):\n\(.comment)\n"' 2>/dev/null || echo "No comments.")

$([ -n "${PLAN_CONTENT}" ] && cat <<PLAN_SECTION
## Implementation Plan

${PLAN_CONTENT}
PLAN_SECTION
)

## Instructions

${MODE_INSTRUCTIONS}
PROMPT_EOF

echo "  Prompt written to ${PROMPT_FILE}"

# --- Load system prompt template (base + mode-specific) ---
#
# Always load the base system-prompt.md first — it carries cross-mode
# guidance (subagent delegation, `### Context Summary` format and mandatory
# emission cadence, code-quality rules). Then append the mode-specific file
# so the mode's prescriptive rules sit on top.
#
# Rationale: with the previous `if/elif` the mode-specific file REPLACED the
# base, so step/plan/fix agents never received the Context Summary guidance
# that the chain-sessions design depends on — resulting in the fallback
# summarizer (a second `claude -p` call) firing on every max-turns hit.

SYSTEM_PROMPT=""
if [ -f "/home/agent/system-prompt.md" ]; then
    SYSTEM_PROMPT=$(cat /home/agent/system-prompt.md)
fi
SYSTEM_PROMPT_FILE="/home/agent/system-prompt-${MODE}.md"
if [ -f "${SYSTEM_PROMPT_FILE}" ]; then
    if [ -n "${SYSTEM_PROMPT}" ]; then
        SYSTEM_PROMPT="${SYSTEM_PROMPT}

# Mode-Specific Instructions: ${MODE}

$(cat "${SYSTEM_PROMPT_FILE}")"
        echo "  Using base + mode-specific system prompt: ${SYSTEM_PROMPT_FILE}"
    else
        SYSTEM_PROMPT=$(cat "${SYSTEM_PROMPT_FILE}")
        echo "  Using mode-specific system prompt only (no base): ${SYSTEM_PROMPT_FILE}"
    fi
elif [ -n "${SYSTEM_PROMPT}" ]; then
    echo "  Using default system prompt"
fi

# --- Append CLAUDE.md to system prompt for prompt caching ---
# Claude Code caches the system prompt prefix across all turns in a session.
# By including CLAUDE.md here (instead of letting the agent read it via a tool
# call), the codebase context stays in the cached prefix from turn 1 — saving
# significant input tokens on every subsequent turn.

CLAUDE_MD="${WORKSPACE}/CLAUDE.md"
if [ -f "${CLAUDE_MD}" ]; then
    CLAUDE_MD_SIZE=$(wc -c < "${CLAUDE_MD}")
    SYSTEM_PROMPT="${SYSTEM_PROMPT}

# Codebase Context (CLAUDE.md)

$(cat "${CLAUDE_MD}")"
    echo "  CLAUDE.md appended to system prompt (${CLAUDE_MD_SIZE} bytes)"
fi

# --- Invoke Claude Code ---

# Copy credentials into the writable .claude directory (mounted as emptyDir).
# The secret is mounted separately; we copy the file so Claude Code has a
# fully writable ~/.claude for session-env and other runtime files.
CRED_SECRET="/home/agent/.claude-secret/.credentials.json"
if [ -f "${CRED_SECRET}" ]; then
    cp "${CRED_SECRET}" /home/agent/.claude/.credentials.json
    echo "  Credentials copied to ~/.claude/"
fi
mkdir -p /home/agent/.claude/session-env

# --- Install Claude Code hooks ---
#
# PreToolUse hook on Bash: pre-executes the command, truncates head+tail
# if output exceeds ~8 KB, returns the trimmed result in place of the
# builtin call. Motivation: builtin Bash returns the full stdout/stderr,
# which lands in the conversation and is cache-read on every subsequent
# turn. Noisy commands (`go test ./...`, `helm lint`, long logs) were
# consistently adding 10-50 KB per call × 20+ turns = 200K-1M wasted
# tokens per step. The hook caps each result at ~4 KB with a truncation
# marker pointing the agent at narrower filters (grep, head, tail).
#
# Configured via `~/.claude/settings.json`. Hook script baked into the
# image at /home/agent/hooks/bash-truncate.py.
if [ -f /home/agent/hooks/bash-truncate.py ]; then
    cat > /home/agent/.claude/settings.json <<'HOOK_EOF'
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/home/agent/hooks/bash-truncate.py",
            "timeout": 150
          }
        ]
      }
    ]
  }
}
HOOK_EOF
    echo "  Installed PreToolUse Bash-truncation hook"
fi

echo "Invoking Claude Code..."

CLAUDE_ARGS=(
    -p
    --dangerously-skip-permissions
    --output-format stream-json
    --verbose
)

if [ -n "${CLAUDE_MODEL:-}" ]; then
    CLAUDE_ARGS+=(--model "${CLAUDE_MODEL}")
    echo "  Model: ${CLAUDE_MODEL}"
fi

if [ -n "${SYSTEM_PROMPT}" ]; then
    CLAUDE_ARGS+=(--system-prompt "${SYSTEM_PROMPT}")
fi

# Build allowed tools flag
if [ -n "${ALLOWED_TOOLS}" ]; then
    CLAUDE_ARGS+=(--allowedTools "${ALLOWED_TOOLS}")
fi

# --- Session chaining ---
#
# Running one long Claude session accumulates context across all turns.
# At 200+ turns the cache reads alone can cost $20+.  Session chaining
# breaks the work into bounded sessions and resets growth, at the cost
# of a fresh-session re-orientation penalty (agent re-reads edited files
# for post-edit state, re-runs builds, etc.) that empirically averages
# ~150–300K tokens per session break.
#
# TURNS_PER_SESSION was 20 historically, but measurements showed most
# step-mode tasks need 50–60 turns — forcing 3 sessions per step and
# paying the break penalty twice. Raised to 30 so medium tasks fit in
# 2 sessions (one break); megasessions are still avoided.
#
# After each session the bootstrap extracts a context summary from the
# last assistant message and feeds it into the NEXT session's prompt,
# so the agent picks up with a fresh, compact context.
#
# The task is done when the agent writes completion-status.json.

TURNS_PER_SESSION="${TURNS_PER_SESSION:-30}"
MAX_SESSIONS="${MAX_SESSIONS:-50}"

# extract_context_summary <result_file>
# Returns the most recent "### Context Summary" block found anywhere in
# the session's assistant text. Scans newest-first so a periodic summary
# at turn 30 is preferred over a stale one at turn 5. Returns empty when
# no summary exists — callers (see continue_session below) then fall
# back to a Claude-generated summary built from a session digest.
#
# Why scan all turns instead of just the last assistant message:
#   When max-turns hits mid-tool-call, the final assistant message is
#   often a tool_use envelope with no text — so the summary written
#   minutes earlier would be lost. Scanning the whole stream recovers it.
extract_context_summary() {
    python3 -c "
import json, sys

assistant_texts = []
for line in open('$1'):
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except:
        continue
    if msg.get('type') != 'assistant':
        continue
    for block in msg.get('message', {}).get('content', []):
        if block.get('type') == 'text' and block.get('text'):
            assistant_texts.append(block['text'])

for text in reversed(assistant_texts):
    idx = text.rfind('### Context Summary')
    if idx >= 0:
        print(text[idx:])
        break
" 2>/dev/null || true
}

# generate_context_summary <session_result_file>
# Produce a `### Context Summary` block for the next chained session.
#
# Previously this piped the session digest to a fresh `claude -p` call
# that paraphrased it. That summarizer burned 30-50K tokens per firing
# AND produced output inferior to what the tool trace already contains:
# the real value for the next session is (a) the list of files already
# read — so it does not re-Read them — and (b) the prior session's
# last narrative / intent. Both are mechanically extractable from the
# stream-json without any LLM. The deterministic version lives in
# summarize-session.py's `--format summary-block` mode.
#
# We keep the function name so callers do not change; the semantics are
# just "build a Context Summary block from the session result, cheaply,
# no Claude call".
generate_context_summary() {
    local session_result="$1"
    [ -f "${session_result}" ] || return 0

    local summary
    summary=$(python3 /home/agent/summarize-session.py \
        --format summary-block "${session_result}" 2>/dev/null) || return 0

    # Sanity-check the block starts where callers expect — on malformed
    # input the extractor returns a stub block; on success it is a real
    # `### Context Summary` header.
    if printf '%s' "${summary}" | grep -q '^### Context Summary'; then
        printf '%s' "${summary}"
    fi
}

echo "  Claude args: ${CLAUDE_ARGS[*]}"
echo "  Prompt size: $(wc -c < "${PROMPT_FILE}") bytes"
echo "  Session chaining: ${TURNS_PER_SESSION} turns/session, max ${MAX_SESSIONS} sessions"

# push_metrics_to_pushgateway <result_file>
# Parses the cumulative stream-json result and pushes Prometheus metrics to
# the Pushgateway. No-op when PUSHGATEWAY_URL is unset (disables metrics
# locally and in tests). Safe to call repeatedly during the run — the
# Pushgateway replaces the group for the same label set, so the latest push
# always reflects the most recent state.
push_metrics_to_pushgateway() {
    local result_file="$1"
    if [ -z "${PUSHGATEWAY_URL:-}" ]; then
        return 0
    fi
    local metrics_file="/tmp/agent-metrics.prom"
    local duration=$((SECONDS - CLAUDE_START_SECONDS))

    if ! python3 /home/agent/parse-usage.py "${result_file}" \
            --format prometheus --duration "${duration}" \
            > "${metrics_file}" 2>/dev/null; then
        return 0
    fi

    # Pushgateway grouping path: job name first, then key/value pairs of
    # identity labels. Labels with empty values would break the URL, so
    # each optional label is appended only when set.
    local url="${PUSHGATEWAY_URL}/metrics/job/agent-worker"
    url="${url}/ticket_id/${TICKET_ID}"
    url="${url}/agent_id/${AGENT_ID}"
    url="${url}/mode/${MODE}"
    url="${url}/specialization/${AGENT_SPECIALIZATION}"
    if [ -n "${PLAN_STEP:-}" ]; then
        url="${url}/plan_step/${PLAN_STEP}"
    fi
    if [ -n "${REPO_NAME:-}" ]; then
        url="${url}/repo/${REPO_NAME}"
    fi

    if ! curl -sf -X POST --data-binary "@${metrics_file}" "${url}" > /dev/null 2>&1; then
        echo "WARNING: Failed to push metrics to ${PUSHGATEWAY_URL}"
    fi
    rm -f "${metrics_file}"
}

CLAUDE_EXIT=0
SESSION_NUM=0
CUMULATIVE_RESULT="${RESULT_FILE}"
SESSION_CONTEXT=""
CLAUDE_START_SECONDS=${SECONDS}
CLAUDE_START_EPOCH=$(date -u +%s)
> "${CUMULATIVE_RESULT}"  # initialize empty

# is_rate_limited <file>
# Returns 0 (match) when the Claude CLI output indicates a rate-limit,
# quota, overload, or billing failure — conditions where retrying later
# is the correct response. These signatures come from the Anthropic API
# error envelopes and Claude Code CLI error text, not from any arbitrary
# occurrence of "rate" or "429" in the agent's own work.
is_rate_limited() {
    local file="$1"
    [ -f "${file}" ] || return 1
    grep -qiE '"(rate_limit_error|overloaded_error)"|"status":[[:space:]]*429|Claude AI usage limit reached|You.?ve hit your limit|credit balance is too low|anthropic-ratelimit-' "${file}"
}

# extract_failure_reason <result_file>
# Distills a short, human-readable line describing why the Claude CLI
# produced nothing useful. Scans stream-json for `{type:result,is_error}`
# or `{type:error}` envelopes, falls back to raw non-JSON lines, and
# finally to the tail of the file. Empty file → a fixed message citing
# CLAUDE_EXIT. Used by the silent-failure guard so humans see the actual
# reason rather than an opaque "Status: unknown".
extract_failure_reason() {
    local file="$1"
    if [ ! -s "${file}" ]; then
        echo "Claude CLI produced no output (exit ${CLAUDE_EXIT})"
        return
    fi
    python3 - "${file}" <<'PY'
import json, sys
path = sys.argv[1]
err = None
for line in open(path):
    line = line.strip()
    if not line:
        continue
    try:
        m = json.loads(line)
    except Exception:
        err = line[:400]
        break
    if m.get('type') == 'result' and m.get('is_error'):
        err = (m.get('result') or m.get('error') or 'unknown error')[:400]
        break
    if m.get('type') == 'error':
        err = str(m.get('message') or m.get('error') or m)[:400]
        break
if not err:
    try:
        with open(path, 'rb') as f:
            f.seek(0, 2)
            size = f.tell()
            f.seek(max(0, size - 2000))
            err = f.read().decode('utf-8', 'replace').strip()[-400:]
    except Exception:
        err = ''
print(err or 'no diagnostic available')
PY
}

while [ "${SESSION_NUM}" -lt "${MAX_SESSIONS}" ]; do
    SESSION_NUM=$((SESSION_NUM + 1))

    # Build per-session prompt.
    #
    # Session 1 uses the full task prompt (ticket context + plan + mode
    # instructions). Session 2+ uses a SLIM continuation prompt instead of
    # re-embedding the full task prompt alongside SESSION_CONTEXT.
    #
    # The previous behavior — `cat PROMPT_FILE + SESSION_CONTEXT` — double-
    # represented prior work: every chained session paid the full ticket
    # description + comments + plan uncached, *and* the context summary on
    # top. On long-running tickets that repeats tens of kilobytes of stale
    # context across every chained session.
    #
    # The slim prompt keeps the load-bearing items only: ticket header,
    # mode-specific completion instructions (the agent needs to know the
    # completion-status.json shape), and the context summary. IMPLEMENTATION_PLAN.md
    # is a workspace file the agent can Read on demand.
    SESSION_PROMPT_FILE="/home/agent/session-prompt.md"
    if [ -n "${SESSION_CONTEXT}" ]; then
        # Pre-compute concrete workspace state so the agent has ZERO reason
        # to re-probe. Observed across multiple step runs: despite explicit
        # "do not re-orient" directives, the very first tool call of every
        # chained session was still `git status && git branch`. Claude's
        # trained orientation habit overrides prose instructions — but if
        # the actual output is already inline, the probe becomes a no-op
        # the agent can skip without effort.
        #
        # Recomputed fresh on each loop iteration — prior session may have
        # committed, pushed, or changed files.
        GIT_CURRENT_BRANCH_FRESH=$(git branch --show-current 2>/dev/null || echo "unknown")
        GIT_PORCELAIN=$(git status --porcelain 2>/dev/null || true)
        if [ -z "${GIT_PORCELAIN}" ]; then
            GIT_TREE_STATE="clean (nothing to commit)"
        else
            GIT_TREE_STATE=$'modified/untracked:\n'"${GIT_PORCELAIN}"
        fi
        # Prefer commits ahead of the work branch for step/fix mode so the
        # log shows THIS step's contribution rather than the cumulative
        # ticket history. Falls back to the base branch when the work
        # branch is unknown (analysis/plan modes).
        GIT_LOG_COMPARE_REF=""
        if [ -n "${WORK_BRANCH:-}" ] && git rev-parse --verify "origin/${WORK_BRANCH}" >/dev/null 2>&1; then
            GIT_LOG_COMPARE_REF="origin/${WORK_BRANCH}"
        elif git rev-parse --verify "origin/${BASE_BRANCH}" >/dev/null 2>&1; then
            GIT_LOG_COMPARE_REF="origin/${BASE_BRANCH}"
        fi
        if [ -n "${GIT_LOG_COMPARE_REF}" ]; then
            GIT_LOG_AHEAD=$(git log --oneline "${GIT_LOG_COMPARE_REF}..HEAD" 2>/dev/null | head -20 || true)
            [ -z "${GIT_LOG_AHEAD}" ] && GIT_LOG_AHEAD="(no commits ahead of ${GIT_LOG_COMPARE_REF})"
            GIT_LOG_LABEL="Commits on this branch ahead of \`${GIT_LOG_COMPARE_REF}\`"
        else
            GIT_LOG_AHEAD=$(git log --oneline -10 2>/dev/null || true)
            GIT_LOG_LABEL="Last 10 commits on this branch"
        fi
        # Cumulative diff of THIS step's changes (committed + uncommitted)
        # vs the work branch. Embedding it in the slim prompt eliminates the
        # biggest chained-session waste pattern: the next session re-Reads
        # the files the prior session edited, just to see the post-edit
        # state. Observed real cost: ~800K cache-read tokens per step for
        # Go-interface work. With the diff inline the agent can Edit
        # directly without re-reading.
        #
        # Caps: 40K chars (~10K tokens) total — generous enough for a
        # typical step's diff, bounded enough not to dominate the slim
        # prompt. Falls back to a `--stat` only when the full diff
        # exceeds the cap, pointing the agent at `git diff <file>` for
        # specific deep-dives.
        GIT_DIFF_SECTION=""
        GIT_DIFF_CAP=40000
        if [ -n "${GIT_LOG_COMPARE_REF}" ]; then
            GIT_DIFF_FULL=$(git diff "${GIT_LOG_COMPARE_REF}" 2>/dev/null || true)
            if [ -n "${GIT_DIFF_FULL}" ]; then
                if [ "$(printf '%s' "${GIT_DIFF_FULL}" | wc -c)" -le "${GIT_DIFF_CAP}" ]; then
                    GIT_DIFF_BLOCK="${GIT_DIFF_FULL}"
                    GIT_DIFF_NOTE="Complete diff of THIS step's work vs \`${GIT_LOG_COMPARE_REF}\` (committed + uncommitted). This IS the post-edit state — do NOT re-Read files to see what the prior session changed."
                else
                    # Too big — stat only, agent can ask for specific diffs.
                    GIT_DIFF_BLOCK=$(git diff --stat "${GIT_LOG_COMPARE_REF}" 2>/dev/null | head -40 || true)
                    GIT_DIFF_NOTE="Diff summary of THIS step's work vs \`${GIT_LOG_COMPARE_REF}\` (full diff exceeded ${GIT_DIFF_CAP} chars). Use \`git diff ${GIT_LOG_COMPARE_REF} -- <file>\` for a specific file. Do NOT re-Read entire files — use targeted diffs."
                fi
                GIT_DIFF_SECTION=$(cat <<DIFF_EOF

## Cumulative Diff of This Step's Work

_${GIT_DIFF_NOTE}_

\`\`\`diff
${GIT_DIFF_BLOCK}
\`\`\`
DIFF_EOF
)
            fi
        fi
        # Include the current step's plan content when available — session-1
        # had it inline in PROMPT_FILE, but sessions 2+ previously had to
        # `Read` IMPLEMENTATION_PLAN.md on every restart.
        PLAN_SECTION=""
        if [ -n "${PLAN_CONTENT}" ]; then
            PLAN_SECTION=$(cat <<PLAN_EOF

## Current Step (from IMPLEMENTATION_PLAN.md, sliced for this step only)

${PLAN_CONTENT}
PLAN_EOF
)
        fi

        cat > "${SESSION_PROMPT_FILE}" <<SLIM_EOF
# Task Continuation

## Agent Identity
- **Agent:** ${AGENT_ID}
- **Mode:** ${MODE}

## Ticket
- **ID:** ${TICKET_ID}
- **Subject:** ${TICKET_SUBJECT}
- **Tags:** ${TICKET_TAGS:-none}
$([ -n "${PLAN_STEP}" ] && echo "- **Plan Step:** ${PLAN_STEP}" || true)

## Verified Workspace State (as of this session start)

These values were captured by the bootstrap RIGHT BEFORE invoking you.
They are authoritative — do not re-verify.

- **cwd:** \`$(pwd)\` (already the cloned repo)
- **Current branch:** \`${GIT_CURRENT_BRANCH_FRESH}\` (already checked out)
- **Working tree:** ${GIT_TREE_STATE}
- **${GIT_LOG_LABEL}:**
\`\`\`
${GIT_LOG_AHEAD}
\`\`\`
${PLAN_SECTION}${GIT_DIFF_SECTION}

## Summary From Previous Session

${SESSION_CONTEXT}

## How To Proceed

Go DIRECTLY to the action described in **Next** above. You already have:

- your branch (checked out)
- working-tree state (above)
- the commits you've already made (above)
- the current step's plan (above, if applicable)
- the **complete diff** of the prior session's edits (above, if applicable)
- what was done last session (in the summary)

Do NOT run \`git status\`, \`git branch\`, \`git log\`, \`git diff\`, or \`ls\` as
your first turn — those outputs are already in this prompt. Every
re-orientation turn costs ~30K cached tokens and is your largest
avoidable waste.

### Do NOT Re-Read Files Whose Post-Edit State Is In The Diff Above

Observed pattern (real, measured): the next session re-Reads each file
the prior session edited, just to see the post-edit state, then re-reads
the unchanged reference files it "wants to double-check", then runs
\`go build\` / \`npm test\` / \`helm lint\` to verify things the prior
session already verified. For a mid-sized step this alone burns ~800K
cache-read tokens. The cumulative diff above is the authoritative
post-edit state — trust it.

Rules (mechanical, not advisory):

1. If a file appears in the diff above, you already have its post-edit
   state. Go directly to Edit if you need further changes. Do NOT Read
   it "to see the current content" — that content is IN the diff.
2. If a file appears in the summary's **Files already READ** list AND
   NOT in the diff above, it is unchanged since the prior session read
   it. The prior read was ingested once; do NOT re-Read.
3. If the prior session's last tool calls show a successful build / test
   run, do NOT re-run the same build / test as your first action. Only
   re-run after you have made NEW edits.
4. If you genuinely need the current state of a specific untouched
   file, use \`git show HEAD:<file>\` (which is cache-efficient) instead
   of a full \`Read\` of the working-tree copy.

If state is truly ambiguous, ask ONE specific question via \`git diff <file>\`
or \`git show <ref>:<file>\` — not a broad probe.

## Completion Instructions

${MODE_INSTRUCTIONS}
SLIM_EOF
        echo "  Session ${SESSION_NUM}: continuing with $(wc -c < "${SESSION_PROMPT_FILE}") bytes (slim prompt with verified state)"
    else
        cp "${PROMPT_FILE}" "${SESSION_PROMPT_FILE}"
        echo "  Session ${SESSION_NUM}: initial session ($(wc -c < "${SESSION_PROMPT_FILE}") bytes)"
    fi

    # Run Claude with a per-session turn limit
    SESSION_RESULT="/home/agent/session-result-${SESSION_NUM}.json"
    CLAUDE_EXIT=0
    claude "${CLAUDE_ARGS[@]}" --max-turns "${TURNS_PER_SESSION}" \
        < "${SESSION_PROMPT_FILE}" > "${SESSION_RESULT}" 2>&1 || CLAUDE_EXIT=$?

    echo "  Session ${SESSION_NUM} finished (exit ${CLAUDE_EXIT}, $(wc -c < "${SESSION_RESULT}" 2>/dev/null || echo 0) bytes)"

    # Append session result to cumulative result for usage tracking
    cat "${SESSION_RESULT}" >> "${CUMULATIVE_RESULT}"

    # Push metrics after every session so the Pushgateway always holds the
    # latest state — if the pod is killed before the task completes, the
    # partial usage is still captured.
    push_metrics_to_pushgateway "${CUMULATIVE_RESULT}"

    # Check if the agent completed its task
    if [ -f "/home/agent/completion-status.json" ]; then
        echo "  Task completed in session ${SESSION_NUM}."
        break
    fi

    # Onestep early-exit: a one-step task whose first full session produces
    # ZERO file edits and ZERO commits is almost certainly misclassified.
    # Every chained session after this re-reads the same files (the slim
    # prompt's cumulative diff is empty until something is committed), and
    # we have measured real runs burning 8 sessions × 30 turns × ~40K
    # cached tokens per turn = ~10M cache-read tokens on re-exploration.
    # Bail with an [analysis:onestep-rejected] marker so the orchestrator
    # hands back to the human instead of respawning onestep over and over.
    if [ "${MODE}" = "onestep" ] && [ "${SESSION_NUM}" = "1" ]; then
        ONESTEP_DIRTY=$(git status --porcelain 2>/dev/null | head -c1)
        ONESTEP_COMMITS_AHEAD=0
        if git rev-parse --verify "origin/${WORK_BRANCH}" >/dev/null 2>&1; then
            ONESTEP_COMMITS_AHEAD=$(git rev-list --count "origin/${WORK_BRANCH}..HEAD" 2>/dev/null || echo 0)
        fi
        if [ -z "${ONESTEP_DIRTY}" ] && [ "${ONESTEP_COMMITS_AHEAD}" = "0" ]; then
            echo "Onestep session 1 ended with zero progress — handing back as scope-too-large."
            # request_human_input posts the comment (marker on line 1 so
            # determineMode() sees it), reassigns to human, and exits 0.
            request_human_input "[analysis:onestep-rejected]\n\n**Scope exceeds one-step contract — no progress in session 1.**\n\nSession 1 (${TURNS_PER_SESSION} turns) produced no file edits and no commits. One-step tickets must be small enough to complete end-to-end in a single Claude session; when the whole first session goes to exploration and reference study, the ticket needs a multi-step plan instead.\n\n**Suggestion:** remove the \`one-step\` tag and move the ticket back to \`ready\` for normal analysis + planning, or split the ticket into smaller tickets."
        fi
    fi

    # Non-zero exit without completion means Claude hit the turn limit
    # or encountered an issue — derive a compact summary so the next
    # session can continue from a real checkpoint instead of restarting.
    SESSION_CONTEXT=$(extract_context_summary "${SESSION_RESULT}")
    if [ -z "${SESSION_CONTEXT}" ]; then
        echo "  No '### Context Summary' block in session ${SESSION_NUM} —"
        echo "  invoking summarizer to compress the transcript before the next session."
        SESSION_CONTEXT=$(generate_context_summary "${SESSION_RESULT}")
    fi
    if [ -z "${SESSION_CONTEXT}" ]; then
        echo "  WARNING: Summarizer produced no output. Stopping session loop."
        break
    fi
    echo "  Session ${SESSION_NUM}: extracted context ($(echo "${SESSION_CONTEXT}" | wc -c) bytes)"
done

if [ "${SESSION_NUM}" -ge "${MAX_SESSIONS}" ] && [ ! -f "/home/agent/completion-status.json" ]; then
    echo "  WARNING: Reached max sessions (${MAX_SESSIONS}) without completing task."
fi

echo "Claude Code finished after ${SESSION_NUM} session(s) (last exit code: ${CLAUDE_EXIT})."

# Show a summary of Claude's output for debugging
RESULT_SIZE=$(wc -c < "${CUMULATIVE_RESULT}" 2>/dev/null || echo "0")
echo "  Result file: ${RESULT_SIZE} bytes"
if [ "${RESULT_SIZE}" -lt 500 ]; then
    echo "  Result content:"
    cat "${CUMULATIVE_RESULT}" || true
elif [ "${CLAUDE_EXIT}" -ne 0 ]; then
    echo "  Last 20 lines of result:"
    tail -20 "${CUMULATIVE_RESULT}" || true
fi

# --- Extract token usage + push final metrics ---

USAGE_SUMMARY=""
if [ -f "${RESULT_FILE}" ]; then
    USAGE_SUMMARY=$(python3 /home/agent/parse-usage.py "${RESULT_FILE}" \
        --format markdown 2>/dev/null || true)

    if [ -n "${USAGE_SUMMARY}" ]; then
        echo "Token usage:"
        echo "${USAGE_SUMMARY}"
    fi

    # Final push — includes the completed duration now that the loop exited.
    push_metrics_to_pushgateway "${RESULT_FILE}"
fi

# --- Export transcript for Claude Spend ---
#
# The orchestrator mounts /var/lib/dev-env/claude-spend from the host at
# /home/agent/claude-spend-out, mirroring what `npx claude-spend` expects
# to find in `~/.claude/`. Two files are produced per run:
#
#   * projects/<project>/<session>.jsonl — the transcript (user/assistant
#     events with synthetic timestamps)
#   * history.jsonl                       — a flat, cross-project index
#     that maps sessionId → friendly label; claude-spend uses it as the
#     display prompt for each session in its UI
#
# Session filenames are unique per agent/ticket/mode/timestamp so parallel
# agents never collide.
CLAUDE_SPEND_OUT_DIR="/home/agent/claude-spend-out"
if [ -f "${RESULT_FILE}" ] && [ -d "${CLAUDE_SPEND_OUT_DIR}" ]; then
    CS_PROJECT="${REPO_NAME:-parts-unlimited-agents}"
    CS_SESSION="${AGENT_ID}-ticket-${TICKET_ID}-${MODE}-$(date -u +%Y%m%dT%H%M%SZ)"
    CS_PATH="${CLAUDE_SPEND_OUT_DIR}/projects/${CS_PROJECT}/${CS_SESSION}.jsonl"
    if python3 /home/agent/export-claude-spend.py \
            "${RESULT_FILE}" "${CS_PATH}" \
            --start-epoch "${CLAUDE_START_EPOCH:-$(date -u +%s)}" 2>&1; then
        # Append a history.jsonl entry so the Claude Spend UI shows a
        # meaningful label for the run (otherwise it falls back to the
        # first raw user prompt, which for agents is a very long
        # system-prompt/task blob).
        #
        # `sessionId` must exactly equal the transcript filename minus
        # `.jsonl` — claude-spend uses it as the key. jq safely escapes
        # the label so special characters in TICKET_SUBJECT don't break
        # the JSONL.
        CS_HISTORY_FILE="${CLAUDE_SPEND_OUT_DIR}/history.jsonl"
        # Build "[step 2]" / "[plan]" / "[fix]" / "[analysis]" — never
        # "[step step 2]" (PLAN_STEP only applies in step mode anyway).
        if [ "${MODE}" = "step" ] && [ -n "${PLAN_STEP:-}" ]; then
            CS_MODE_LABEL="step ${PLAN_STEP}"
        else
            CS_MODE_LABEL="${MODE}"
        fi
        CS_DISPLAY="[${CS_MODE_LABEL}] Ticket #${TICKET_ID}: ${TICKET_SUBJECT:-<no subject>}"
        if ! jq -cn \
                --arg sid "${CS_SESSION}" \
                --arg display "${CS_DISPLAY}" \
                '{sessionId: $sid, display: $display}' \
                >> "${CS_HISTORY_FILE}"; then
            echo "WARNING: Failed to append Claude Spend history entry."
        fi
    else
        echo "WARNING: Failed to export transcript for Claude Spend."
    fi
fi


# --- Rate-limit short-circuit ---
#
# When Claude hits rate limits, quota exhaustion, model overload, or
# billing errors, the agent has done no real work and must not leave any
# trace (Taiga comment, git push, PR) — those side effects would confuse
# the orchestrator into thinking work happened. Exit cleanly so the K8s
# Job completes; after its TTLSecondsAfterFinished elapses the
# orchestrator's reconcile loop will re-spawn the agent, by which time
# the limit has usually cleared.
#
# Skip this when completion-status.json exists: the agent successfully
# finished its task before any transient API errors, so normal
# post-processing should run.
if [ ! -f "/home/agent/completion-status.json" ] && is_rate_limited "${RESULT_FILE}"; then
    echo "Claude hit a rate-limit / quota / overload condition — no work to post."
    echo "  Skipping Taiga comment, git push, and PR creation."
    echo "  Exiting 0 so the Job completes cleanly; the orchestrator will reschedule"
    echo "  via reconciliation once the K8s Job TTL expires."
    exit 0
fi

# --- Process result ---

COMPLETION_STATUS="unknown"
COMPLETION_SUMMARY=""

if [ -f "/home/agent/completion-status.json" ]; then
    COMPLETION_STATUS=$(jq -r '.status' /home/agent/completion-status.json)
    COMPLETION_SUMMARY=$(jq -r '.summary // ""' /home/agent/completion-status.json)
fi

echo "  Completion status: ${COMPLETION_STATUS}"
echo "  Summary: ${COMPLETION_SUMMARY}"

# --- Mode-specific post-processing ---

if [ "${MODE}" = "analysis" ]; then
    # --- Analysis mode: post result comment, handle need-info ---

    ANALYSIS_RESULT=$(jq -r '.analysis_result // ""' /home/agent/completion-status.json 2>/dev/null || true)
    ANALYSIS_COMMENT=$(jq -r '.analysis_comment // ""' /home/agent/completion-status.json 2>/dev/null || true)

    echo "  Analysis result: ${ANALYSIS_RESULT}"

    # Record the canonical repo in the comment. This is the orchestrator's
    # only source of truth for which fork a ticket belongs to (it holds
    # no local state), so every agent comment embeds it.
    if [ -n "${ANALYSIS_COMMENT}" ] && [ -n "${REPO_OWNER:-}" ] && [ -n "${REPO_NAME:-}" ]; then
        ANALYSIS_COMMENT="${ANALYSIS_COMMENT}\n\n**Repo:** \`${REPO_OWNER}/${REPO_NAME}\`"
    fi

    # Append usage summary to analysis comment
    if [ -n "${USAGE_SUMMARY}" ] && [ -n "${ANALYSIS_COMMENT}" ]; then
        ANALYSIS_COMMENT="${ANALYSIS_COMMENT}\n\n### Token Usage\n${USAGE_SUMMARY}"
    fi

    if [ -n "${ANALYSIS_COMMENT}" ]; then
        echo "Posting analysis comment on ticket..."
        CURRENT_VERSION=$(curl -sf -H "${TAIGA_AUTH_HEADER}" \
            "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
            | jq -r '.version')

        curl -sf -X PATCH "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
            -H "${TAIGA_AUTH_HEADER}" \
            -H "Content-Type: application/json" \
            -d "{\"comment\": $(echo "${ANALYSIS_COMMENT}" | jq -Rs .), \"version\": ${CURRENT_VERSION}}" \
            >/dev/null 2>&1 || echo "WARNING: Could not post analysis comment on ticket."
    fi

    if [ "${ANALYSIS_RESULT}" = "need-info" ]; then
        echo "Analysis requires human input — assigning ticket to human."
        request_human_input "Analysis requires additional information. See the analysis comment above for details."
    elif [ "${ANALYSIS_RESULT}" = "onestep-rejected" ]; then
        # The analysis agent disagreed with the \`one-step\` tag.  Hand the
        # ticket back so the human can either remove the tag (allowing a
        # normal multi-step plan) or split the ticket.
        echo "Analysis rejected one-step assignment — assigning ticket to human for reevaluation."
        request_human_input "The analysis agent disagreed with the \`one-step\` tag for this ticket. See the analysis comment above. Please either remove the \`one-step\` tag and move the ticket back to \`ready\` for normal planning, or split the ticket into smaller ones."
    fi

else
    # --- Plan/Step/Fix modes: push, create PR, post comment ---

    # Handle blocked status
    if [ "${COMPLETION_STATUS}" = "blocked" ]; then
        BLOCKED_REASON=$(jq -r '.reason // "No reason provided"' /home/agent/completion-status.json 2>/dev/null || echo "No reason provided")
        BLOCKED_COMMENT=$(jq -r '.taiga_comment // ""' /home/agent/completion-status.json 2>/dev/null || true)
        echo "Agent is blocked — requesting human input."
        # Prefer the agent's own taiga_comment when provided — it carries
        # lifecycle markers (e.g. [analysis:onestep-rejected] from a
        # onestep agent that found the scope too large) that the
        # orchestrator's determineMode() depends on.
        if [ -n "${BLOCKED_COMMENT}" ]; then
            request_human_input "${BLOCKED_COMMENT}"
        else
            request_human_input "**Agent ${AGENT_ID}** is blocked and needs human input.\n\n**Reason:** ${BLOCKED_REASON}"
        fi
    fi

    # --- Silent-failure guard ---
    #
    # If the agent produced no new commits (and no uncommitted changes),
    # the Claude CLI session almost certainly failed silently (auth
    # error, crash, unrecognised rate-limit variant, etc.). Creating an
    # empty PR or posting a `[step:in-progress]` comment in that case is
    # dishonest and has produced a stream of no-op PRs and misleading
    # ticket history. Surface the underlying failure and hand off.
    HAS_NEW_WORK="false"
    if [ -n "${BRANCH_NAME}" ]; then
        if [ -n "$(git status --porcelain)" ]; then
            HAS_NEW_WORK="true"
        elif [ "${MODE}" = "fix" ]; then
            # Fix mode works on an existing PR branch; new work shows up
            # as local commits not yet pushed to that same remote branch.
            LOCAL_HEAD=$(git rev-parse HEAD)
            REMOTE_HEAD=$(git rev-parse "origin/${BRANCH_NAME}" 2>/dev/null || echo "")
            if [ -n "${REMOTE_HEAD}" ] && [ "${LOCAL_HEAD}" != "${REMOTE_HEAD}" ]; then
                HAS_NEW_WORK="true"
            fi
        else
            # Plan/step branches are cut from WORK_BRANCH — any real work
            # shows up as commits ahead of it.
            COMMITS_AHEAD=$(git rev-list --count "origin/${WORK_BRANCH}..HEAD" 2>/dev/null || echo 0)
            if [ "${COMMITS_AHEAD}" != "0" ]; then
                HAS_NEW_WORK="true"
            fi
        fi
    fi

    if [ "${HAS_NEW_WORK}" = "false" ]; then
        FAILURE_REASON=$(extract_failure_reason "${CUMULATIVE_RESULT}")
        echo "Agent produced no new commits — treating as silent failure."
        echo "  Claude exit:  ${CLAUDE_EXIT}"
        echo "  Diagnostic:   ${FAILURE_REASON}"
        request_human_input "**Agent \`${AGENT_ID}\`** produced no commits on ticket #${TICKET_ID} (branch \`${BRANCH_NAME:-N/A}\`, mode \`${MODE}\`).

Claude CLI exit: \`${CLAUDE_EXIT}\`

**Diagnostic:**
\`\`\`
${FAILURE_REASON}
\`\`\`"
    fi

    # Push changes
    if [ -n "${BRANCH_NAME}" ] && [ -n "$(git status --porcelain)" ]; then
        echo "Pushing changes..."
        git add -A
        git commit -m "Agent ${AGENT_ID}: work on ticket #${TICKET_ID}

${COMPLETION_SUMMARY:-Work in progress}"
        git push -u origin "${BRANCH_NAME}"
        echo "  Pushed to ${BRANCH_NAME}"
    elif [ -n "${BRANCH_NAME}" ]; then
        # Check if there are unpushed commits
        LOCAL_HEAD=$(git rev-parse HEAD)
        REMOTE_HEAD=$(git rev-parse "origin/${BRANCH_NAME}" 2>/dev/null || echo "")
        if [ "${LOCAL_HEAD}" != "${REMOTE_HEAD}" ]; then
            echo "Pushing commits..."
            git push -u origin "${BRANCH_NAME}"
            echo "  Pushed to ${BRANCH_NAME}"
        else
            echo "  No changes to push."
        fi
    fi

    # Create or update PR (skip for fix mode — PR already exists; also
    # skip when the agent ended up on the work branch itself, since
    # work → BASE_BRANCH is a separate human-driven step).
    if [ -n "${BRANCH_NAME}" ] && [ "${MODE}" != "fix" ] && [ "${BRANCH_NAME}" != "${WORK_BRANCH}" ]; then
        echo "Checking for existing PR..."
        EXISTING_PR=$(curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
            "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/pulls?state=open&head=${BRANCH_NAME}" \
            | jq '.[0].number // empty' 2>/dev/null || true)

        PR_BODY="## Ticket
[#${TICKET_ID}: ${TICKET_SUBJECT}](${TAIGA_URL}/project/dev-environment/us/${TICKET_ID})
$([ -n "${PLAN_STEP}" ] && echo "
## Plan Step
Step ${PLAN_STEP}" || true)

## Summary
${COMPLETION_SUMMARY:-Work by agent ${AGENT_ID}}

## Status
${COMPLETION_STATUS}"

        if [ -n "${EXISTING_PR}" ]; then
            echo "  PR #${EXISTING_PR} already exists, updated with latest push."
        else
            echo "Creating PR..."
            # All agent-created PRs target the integration work branch.
            # The work → BASE_BRANCH PR is only opened manually (by a human)
            # once every step PR has been merged, so the agent never
            # produces a PR targeting BASE_BRANCH directly.
            PR_BASE="${WORK_BRANCH}"

            PR_TITLE="Ticket #${TICKET_ID}"
            if [ "${MODE}" = "plan" ]; then
                PR_TITLE="${PR_TITLE}: Implementation Plan"
            elif [ -n "${PLAN_STEP}" ]; then
                PR_TITLE="${PR_TITLE} - Step ${PLAN_STEP}: ${TICKET_SUBJECT}"
            else
                PR_TITLE="${PR_TITLE}: ${TICKET_SUBJECT}"
            fi

            PR_RESPONSE=$(curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
                -X POST "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/pulls" \
                -H "Content-Type: application/json" \
                -d "{
                    \"title\": $(echo "${PR_TITLE}" | jq -Rs .),
                    \"body\": $(echo "${PR_BODY}" | jq -Rs .),
                    \"head\": \"${BRANCH_NAME}\",
                    \"base\": \"${PR_BASE}\"
                }" 2>/dev/null || true)

            PR_NUMBER=$(echo "${PR_RESPONSE}" | jq -r '.number // empty' 2>/dev/null || true)
            if [ -n "${PR_NUMBER}" ]; then
                echo "  Created PR #${PR_NUMBER}"
            else
                echo "  WARNING: Could not create PR"
            fi
        fi
    fi

    # Post progress comment on ticket (use taiga_comment from completion-status if available)
    TAIGA_COMMENT=$(jq -r '.taiga_comment // ""' /home/agent/completion-status.json 2>/dev/null || true)
    if [ -z "${TAIGA_COMMENT}" ]; then
        TAIGA_COMMENT="**Agent ${AGENT_ID}** completed work.\n\n**Status:** ${COMPLETION_STATUS}\n**Summary:** ${COMPLETION_SUMMARY:-N/A}\n**Branch:** \`${BRANCH_NAME:-N/A}\`"
    fi

    # Ensure lifecycle markers are present so the orchestrator can derive
    # the correct mode.  The agent may not have written the expected marker
    # in completion-status.json, so we inject it when a PR exists.
    HAS_PR="false"
    if [ -n "${PR_NUMBER}" ] || [ -n "${EXISTING_PR}" ]; then
        HAS_PR="true"
    fi
    if [ "${HAS_PR}" = "true" ]; then
        case "${MODE}" in
            plan)
                if ! echo "${TAIGA_COMMENT}" | grep -qF '[phase:plan-created]'; then
                    TAIGA_COMMENT="[phase:plan-created]\n\n${TAIGA_COMMENT}"
                fi
                ;;
            step)
                # Overwrite whatever step marker the LLM wrote with one
                # derived from authoritative sources. The agent has
                # historically picked the wrong N (e.g. ticket #22 went
                # `[step:3/6]` → `[step:1/6]` after a plan rewrite), so
                # we trust merged-PR count + plan heading count instead.
                # `[step:complete]` is a different terminal marker —
                # leave it untouched.
                if ! echo "${TAIGA_COMMENT}" | grep -qF '[step:complete]'; then
                    AUTH_N="${PLAN_STEP}"
                    AUTH_M=$(grep -cE '^### Step [0-9]+' \
                        "${WORKSPACE}/IMPLEMENTATION_PLAN.md" 2>/dev/null || echo 0)
                    if [ "${AUTH_M}" -gt 0 ]; then
                        CANONICAL_STEP_MARKER="[step:${AUTH_N}/${AUTH_M}]"
                        if echo "${TAIGA_COMMENT}" | grep -qE '\[step:[^]]*\]'; then
                            TAIGA_COMMENT=$(printf '%s' "${TAIGA_COMMENT}" \
                                | sed -E "s#\\[step:[^]]*\\]#${CANONICAL_STEP_MARKER}#")
                        else
                            TAIGA_COMMENT="${CANONICAL_STEP_MARKER}\n\n${TAIGA_COMMENT}"
                        fi
                    elif ! echo "${TAIGA_COMMENT}" | grep -qF '[step:'; then
                        # Plan unreadable / missing — fall back to the
                        # in-progress marker so the orchestrator at least
                        # sees *some* step marker.
                        TAIGA_COMMENT="[step:in-progress]\n\n${TAIGA_COMMENT}"
                    fi
                fi
                ;;
            onestep)
                # For onestep the PR itself is the whole implementation —
                # the marker we need is [step:complete] so the orchestrator
                # transitions the ticket to ready-for-test when the PR
                # merges. Inject it if the agent forgot.
                if ! echo "${TAIGA_COMMENT}" | grep -qF '[step:'; then
                    TAIGA_COMMENT="[step:complete]\n\n${TAIGA_COMMENT}"
                fi
                ;;
            fix)
                if ! echo "${TAIGA_COMMENT}" | grep -qF '[fix:applied]'; then
                    TAIGA_COMMENT="[fix:applied]\n\n${TAIGA_COMMENT}"
                fi
                ;;
        esac
    fi

    # Record the canonical repo in the comment. This is the orchestrator's
    # only source of truth for which fork a ticket belongs to (it holds
    # no local state), so every agent comment embeds it.
    if [ -n "${REPO_OWNER:-}" ] && [ -n "${REPO_NAME:-}" ]; then
        TAIGA_COMMENT="${TAIGA_COMMENT}\n\n**Repo:** \`${REPO_OWNER}/${REPO_NAME}\`"
    fi

    # Append usage summary to Taiga comment
    if [ -n "${USAGE_SUMMARY}" ]; then
        TAIGA_COMMENT="${TAIGA_COMMENT}\n\n### Token Usage\n${USAGE_SUMMARY}"
    fi

    echo "Posting progress comment on ticket..."
    CURRENT_VERSION=$(curl -sf -H "${TAIGA_AUTH_HEADER}" \
        "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
        | jq -r '.version')

    curl -sf -X PATCH "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
        -H "${TAIGA_AUTH_HEADER}" \
        -H "Content-Type: application/json" \
        -d "{\"comment\": $(echo "${TAIGA_COMMENT}" | jq -Rs .), \"version\": ${CURRENT_VERSION}}" \
        >/dev/null 2>&1 || echo "WARNING: Could not post comment on ticket."
fi

echo ""
echo "=== Agent Worker Complete ==="
echo "  Status: ${COMPLETION_STATUS}"
