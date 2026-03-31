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

CLAUDE_CREDENTIALS="/home/agent/.claude/.credentials.json"

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
ALLOWED_TOOLS="${ALLOWED_TOOLS:-Read Edit Write Glob Grep Bash}"
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

echo "Fetching ticket comments..."
COMMENTS=$(curl -sf -H "${TAIGA_AUTH_HEADER}" \
    "${TAIGA_URL}/api/v1/history/userstory/${TICKET_ID}" \
    | jq -r '[.[] | select(.comment != null and .comment != "") | {user: .user.username, comment: .comment, date: .created_at}]')
COMMENT_COUNT=$(echo "${COMMENTS}" | jq 'length')
echo "  Comments: ${COMMENT_COUNT}"

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

    # Strip leading/trailing whitespace
    raw=$(echo "${raw}" | sed -E 's/^\s+//;s/\s+$//')

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
    else
        # Local owner/name format
        REPO_OWNER=$(echo "${REPO_REF}" | cut -d/ -f1)
        REPO_NAME=$(echo "${REPO_REF}" | cut -d/ -f2)
        echo "Extracted repo from description: ${REPO_OWNER}/${REPO_NAME}"
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

BASE_BRANCH=$(extract_base_branch "${TICKET_DESCRIPTION}")
BASE_BRANCH="${BASE_BRANCH:-main}"
echo "Base branch: ${BASE_BRANCH}"

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
    fix)
        # Fix mode: check out the existing PR branch (bootstrap determines it later
        # from the PR data).  Set a placeholder — overridden after PR fetch.
        BRANCH_NAME=""
        ;;
    step|*)
        if [ -n "${PLAN_STEP}" ]; then
            BRANCH_NAME="${BRANCH_PREFIX}/step-${PLAN_STEP}"
        else
            BRANCH_NAME="${BRANCH_PREFIX}/work"
        fi
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

cat > "${PROMPT_FILE}" <<PROMPT_EOF
# Task Assignment

## Agent Identity
- **Agent:** ${AGENT_ID}
- **Specialization:** ${AGENT_SPECIALIZATION}
- **Mode:** ${MODE}

## Ticket
- **ID:** ${TICKET_ID}
- **Subject:** ${TICKET_SUBJECT}
$([ -n "${PLAN_STEP}" ] && echo "- **Plan Step:** ${PLAN_STEP}" || true)

## Ticket Description

${TICKET_DESCRIPTION}

## Ticket Comments

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

# --- Load system prompt template (mode-specific) ---

SYSTEM_PROMPT=""
SYSTEM_PROMPT_FILE="/home/agent/system-prompt-${MODE}.md"
if [ -f "${SYSTEM_PROMPT_FILE}" ]; then
    SYSTEM_PROMPT=$(cat "${SYSTEM_PROMPT_FILE}")
    echo "  Using mode-specific system prompt: ${SYSTEM_PROMPT_FILE}"
elif [ -f "/home/agent/system-prompt.md" ]; then
    SYSTEM_PROMPT=$(cat /home/agent/system-prompt.md)
    echo "  Using default system prompt"
fi

# --- Invoke Claude Code ---

echo "Invoking Claude Code..."

CLAUDE_ARGS=(
    -p
    --dangerously-skip-permissions
    --output-format stream-json
)

if [ -n "${SYSTEM_PROMPT}" ]; then
    CLAUDE_ARGS+=(--system-prompt "${SYSTEM_PROMPT}")
fi

# Build allowed tools flag
if [ -n "${ALLOWED_TOOLS}" ]; then
    CLAUDE_ARGS+=(--allowedTools "${ALLOWED_TOOLS}")
fi

# Pipe the prompt via stdin to avoid shell argument length limits.
echo "  Claude args: ${CLAUDE_ARGS[*]}"
echo "  Prompt size: $(wc -c < "${PROMPT_FILE}") bytes"

CLAUDE_EXIT=0
claude "${CLAUDE_ARGS[@]}" < "${PROMPT_FILE}" > "${RESULT_FILE}" 2>&1 || CLAUDE_EXIT=$?

echo "Claude Code finished (exit code: ${CLAUDE_EXIT})."

# Show a summary of Claude's output for debugging
RESULT_SIZE=$(wc -c < "${RESULT_FILE}" 2>/dev/null || echo "0")
echo "  Result file: ${RESULT_SIZE} bytes"
if [ "${RESULT_SIZE}" -lt 500 ]; then
    echo "  Result content:"
    cat "${RESULT_FILE}" || true
elif [ "${CLAUDE_EXIT}" -ne 0 ]; then
    echo "  Last 20 lines of result:"
    tail -20 "${RESULT_FILE}" || true
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
    fi

else
    # --- Plan/Step/Fix modes: push, create PR, post comment ---

    # Handle blocked status
    if [ "${COMPLETION_STATUS}" = "blocked" ]; then
        BLOCKED_REASON=$(jq -r '.reason // "No reason provided"' /home/agent/completion-status.json 2>/dev/null || echo "No reason provided")
        echo "Agent is blocked — requesting human input."
        request_human_input "**Agent ${AGENT_ID}** is blocked and needs human input.\n\n**Reason:** ${BLOCKED_REASON}"
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

    # Create or update PR (skip for fix mode — PR already exists)
    if [ -n "${BRANCH_NAME}" ] && [ "${MODE}" != "fix" ]; then
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
            # Plan/step PRs target the work branch; only the work branch itself targets the base branch.
            if [ "${BRANCH_NAME}" = "${WORK_BRANCH}" ]; then
                PR_BASE="${BASE_BRANCH}"
            else
                PR_BASE="${WORK_BRANCH}"
            fi

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
