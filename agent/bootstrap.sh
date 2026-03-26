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
#   PLAN_STEP              - Implementation plan step number to work on
#   REPO_OWNER             - Gitea repo owner (extracted from ticket if not set)
#   REPO_NAME              - Gitea repo name (extracted from ticket if not set)
#   ALLOWED_TOOLS          - Space-separated list of allowed Claude tools
#   HUMAN_USERNAME         - Taiga username of the human user (for reassignment)

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

PLAN_STEP="${PLAN_STEP:-}"
ALLOWED_TOOLS="${ALLOWED_TOOLS:-Read Edit Write Glob Grep Bash}"
HUMAN_USERNAME="${HUMAN_USERNAME:-}"

# --- Helper: request human input and exit cleanly ---

# Posts a comment on the ticket, reassigns to the human user, and exits 0.
# Usage: request_human_input <comment_text>
request_human_input() {
    local comment="$1"
    local version

    # Fetch current ticket version
    version=$(curl -sf -H "${TAIGA_AUTH_HEADER}" \
        "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
        | jq -r '.version') || true

    if [ -n "${version}" ]; then
        # Post comment
        curl -sf -X PATCH "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
            -H "${TAIGA_AUTH_HEADER}" \
            -H "Content-Type: application/json" \
            -d "{\"comment\": $(echo "${comment}" | jq -Rs .), \"version\": ${version}}" \
            >/dev/null 2>&1 || echo "WARNING: Could not post comment on ticket."

        # Re-fetch version after comment (Taiga increments it)
        version=$(curl -sf -H "${TAIGA_AUTH_HEADER}" \
            "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
            | jq -r '.version') || true

        # Reassign to human user if known
        if [ -n "${HUMAN_USERNAME}" ] && [ -n "${version}" ]; then
            local human_id
            human_id=$(curl -sf -H "${TAIGA_AUTH_HEADER}" \
                "${TAIGA_URL}/api/v1/users?project=${TAIGA_PROJECT_ID:-}" \
                | jq -r ".[] | select(.username == \"${HUMAN_USERNAME}\") | .id" 2>/dev/null) || true

            if [ -n "${human_id}" ]; then
                curl -sf -X PATCH "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
                    -H "${TAIGA_AUTH_HEADER}" \
                    -H "Content-Type: application/json" \
                    -d "{\"assigned_to\": ${human_id}, \"version\": ${version}}" \
                    >/dev/null 2>&1 || echo "WARNING: Could not reassign ticket to ${HUMAN_USERNAME}."
            fi
        fi
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

if [ -n "${REPO_OWNER:-}" ] && [ -n "${REPO_NAME:-}" ]; then
    echo "Using repo from environment: ${REPO_OWNER}/${REPO_NAME}"
else
    # Try to extract repo info from ticket description
    # Expected format: repo owner/name or a Gitea URL
    REPO_MATCH=$(echo "${TICKET_DESCRIPTION}" | grep -oP '(?:repo|gitea)[:\s]+\K\S+/\S+' | head -1 || true)
    if [ -n "${REPO_MATCH}" ]; then
        REPO_OWNER=$(echo "${REPO_MATCH}" | cut -d/ -f1)
        REPO_NAME=$(echo "${REPO_MATCH}" | cut -d/ -f2)
        echo "Extracted repo from description: ${REPO_OWNER}/${REPO_NAME}"
    else
        echo "Cannot determine target repository — requesting human input."
        request_human_input "Cannot determine the target repository. Please specify the repository in the description (e.g., \`repo: owner/name\`) or provide it in a comment."
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

# --- Determine branch ---

BRANCH_PREFIX="agent/${AGENT_ID}/ticket-${TICKET_ID}"
if [ -n "${PLAN_STEP}" ]; then
    BRANCH_NAME="${BRANCH_PREFIX}/step-${PLAN_STEP}"
else
    BRANCH_NAME="${BRANCH_PREFIX}/work"
fi

# Check if branch already exists (resuming previous work)
if git ls-remote --heads origin "${BRANCH_NAME}" | grep -q "${BRANCH_NAME}"; then
    echo "Resuming work on existing branch: ${BRANCH_NAME}"
    git checkout "${BRANCH_NAME}"
else
    echo "Creating new branch: ${BRANCH_NAME}"
    git checkout -b "${BRANCH_NAME}"
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

cat > "${PROMPT_FILE}" <<PROMPT_EOF
# Task Assignment

## Agent Identity
- **Agent:** ${AGENT_ID}
- **Specialization:** ${AGENT_SPECIALIZATION}

## Ticket
- **ID:** ${TICKET_ID}
- **Subject:** ${TICKET_SUBJECT}
$([ -n "${PLAN_STEP}" ] && echo "- **Plan Step:** ${PLAN_STEP}")

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

You are an autonomous coding agent. Your task is to implement the work described in the ticket above.

Guidelines:
- Follow the implementation plan if one exists. Work on step ${PLAN_STEP:-"as described in the ticket"}.
- Write clean, well-documented code following the language's best practices.
- Every public method and class must be documented.
- Never use magic constants — define named constants.
- Use parameterized tests where possible.
- Include sufficient tests to verify your work.
- Run tests and linters before finishing.
- Commit your work with clear, descriptive commit messages.
- Do not push to remote — the bootstrap script handles that.

When you are done, create a file at /home/agent/completion-status.json with:
\`\`\`json
{
  "status": "success",
  "summary": "Brief description of what was done",
  "files_changed": ["list", "of", "files"]
}
\`\`\`

If you encounter a blocking issue, create the file with:
\`\`\`json
{
  "status": "blocked",
  "reason": "Description of the blocking issue"
}
\`\`\`
PROMPT_EOF

echo "  Prompt written to ${PROMPT_FILE}"

# --- Load system prompt template ---

SYSTEM_PROMPT=""
if [ -f "/home/agent/system-prompt.md" ]; then
    SYSTEM_PROMPT=$(cat /home/agent/system-prompt.md)
fi

# --- Invoke Claude Code ---

echo "Invoking Claude Code..."

CLAUDE_ARGS=(
    -p
    --dangerously-skip-permissions
    --bare
    --no-session-persistence
    --output-format json
)

if [ -n "${SYSTEM_PROMPT}" ]; then
    CLAUDE_ARGS+=(--system-prompt "${SYSTEM_PROMPT}")
fi

# Build allowed tools flag
if [ -n "${ALLOWED_TOOLS}" ]; then
    CLAUDE_ARGS+=(--allowedTools "${ALLOWED_TOOLS}")
fi

TASK_PROMPT=$(cat "${PROMPT_FILE}")

claude "${CLAUDE_ARGS[@]}" "${TASK_PROMPT}" > "${RESULT_FILE}" 2>&1 || true

echo "Claude Code finished."

# --- Process result ---

COMPLETION_STATUS="unknown"
COMPLETION_SUMMARY=""

if [ -f "/home/agent/completion-status.json" ]; then
    COMPLETION_STATUS=$(jq -r '.status' /home/agent/completion-status.json)
    COMPLETION_SUMMARY=$(jq -r '.summary // ""' /home/agent/completion-status.json)
fi

echo "  Completion status: ${COMPLETION_STATUS}"
echo "  Summary: ${COMPLETION_SUMMARY}"

# --- Handle blocked status ---

if [ "${COMPLETION_STATUS}" = "blocked" ]; then
    BLOCKED_REASON=$(jq -r '.reason // "No reason provided"' /home/agent/completion-status.json 2>/dev/null || echo "No reason provided")
    echo "Agent is blocked — requesting human input."
    request_human_input "**Agent ${AGENT_ID}** is blocked and needs human input.\n\n**Reason:** ${BLOCKED_REASON}"
fi

# --- Push changes ---

if [ -n "$(git status --porcelain)" ] || [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/$(git rev-parse --abbrev-ref HEAD) 2>/dev/null || echo '')" ]; then
    echo "Pushing changes..."
    # Stage and commit any uncommitted work
    if [ -n "$(git status --porcelain)" ]; then
        git add -A
        git commit -m "Agent ${AGENT_ID}: work on ticket #${TICKET_ID}

${COMPLETION_SUMMARY:-Work in progress}"
    fi
    git push -u origin "${BRANCH_NAME}"
    echo "  Pushed to ${BRANCH_NAME}"
else
    echo "  No changes to push."
fi

# --- Create or update PR ---

echo "Checking for existing PR..."
EXISTING_PR=$(curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
    "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/pulls?state=open&head=${BRANCH_NAME}" \
    | jq '.[0].number // empty' 2>/dev/null || true)

PR_BODY="## Ticket
[#${TICKET_ID}: ${TICKET_SUBJECT}](${TAIGA_URL}/project/dev-environment/us/${TICKET_ID})
$([ -n "${PLAN_STEP}" ] && echo "
## Plan Step
Step ${PLAN_STEP}")

## Summary
${COMPLETION_SUMMARY:-Work by agent ${AGENT_ID}}

## Status
${COMPLETION_STATUS}"

if [ -n "${EXISTING_PR}" ]; then
    echo "  PR #${EXISTING_PR} already exists, updated with latest push."
else
    echo "Creating PR..."
    PR_RESPONSE=$(curl -sf -u "${GITEA_USERNAME}:${GITEA_PASSWORD}" \
        -X POST "${GITEA_URL}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/pulls" \
        -H "Content-Type: application/json" \
        -d "{
            \"title\": \"[${AGENT_ID}] Ticket #${TICKET_ID}$([ -n "${PLAN_STEP}" ] && echo " - Step ${PLAN_STEP}"): ${TICKET_SUBJECT}\",
            \"body\": $(echo "${PR_BODY}" | jq -Rs .),
            \"head\": \"${BRANCH_NAME}\",
            \"base\": \"main\"
        }" 2>/dev/null || true)

    PR_NUMBER=$(echo "${PR_RESPONSE}" | jq -r '.number // empty' 2>/dev/null || true)
    if [ -n "${PR_NUMBER}" ]; then
        echo "  Created PR #${PR_NUMBER}"
    else
        echo "  WARNING: Could not create PR"
    fi
fi

# --- Update ticket with progress comment ---

echo "Posting progress comment on ticket..."
COMMENT_TEXT="**Agent ${AGENT_ID}** completed work.\n\n**Status:** ${COMPLETION_STATUS}\n**Summary:** ${COMPLETION_SUMMARY:-N/A}\n**Branch:** \`${BRANCH_NAME}\`"

# Re-fetch ticket version to avoid conflict
CURRENT_VERSION=$(curl -sf -H "${TAIGA_AUTH_HEADER}" \
    "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
    | jq -r '.version')

curl -sf -X PATCH "${TAIGA_URL}/api/v1/userstories/${TICKET_ID}" \
    -H "${TAIGA_AUTH_HEADER}" \
    -H "Content-Type: application/json" \
    -d "{\"comment\": \"$(echo -e "${COMMENT_TEXT}")\", \"version\": ${CURRENT_VERSION}}" \
    >/dev/null 2>&1 || echo "WARNING: Could not post comment on ticket."

echo ""
echo "=== Agent Worker Complete ==="
echo "  Status: ${COMPLETION_STATUS}"
