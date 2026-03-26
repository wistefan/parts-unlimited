#!/usr/bin/env bash
# Unit tests for the bootstrap script logic.
# Tests environment validation, repo extraction, branch naming, and prompt generation.
# Does NOT require a running Taiga/Gitea instance — tests pure bash logic.

set -euo pipefail

PASS=0
FAIL=0
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

assert_eq() {
    local test_name="$1"
    local expected="$2"
    local actual="$3"

    if [ "${expected}" = "${actual}" ]; then
        echo "  [PASS] ${test_name}"
        PASS=$((PASS + 1))
    else
        echo "  [FAIL] ${test_name}"
        echo "         expected: ${expected}"
        echo "         actual:   ${actual}"
        FAIL=$((FAIL + 1))
    fi
}

assert_contains() {
    local test_name="$1"
    local needle="$2"
    local haystack="$3"

    if echo "${haystack}" | grep -qF "${needle}"; then
        echo "  [PASS] ${test_name}"
        PASS=$((PASS + 1))
    else
        echo "  [FAIL] ${test_name}"
        echo "         expected to contain: ${needle}"
        FAIL=$((FAIL + 1))
    fi
}

echo "=== Bootstrap Logic Tests ==="
echo ""

# --- Test: branch name generation ---
echo "Branch naming:"

AGENT_ID="general-agent-1"
TICKET_ID="42"

PLAN_STEP="3"
BRANCH_PREFIX="agent/${AGENT_ID}/ticket-${TICKET_ID}"
BRANCH_NAME="${BRANCH_PREFIX}/step-${PLAN_STEP}"
assert_eq "branch with plan step" "agent/general-agent-1/ticket-42/step-3" "${BRANCH_NAME}"

PLAN_STEP=""
if [ -n "${PLAN_STEP}" ]; then
    BRANCH_NAME="${BRANCH_PREFIX}/step-${PLAN_STEP}"
else
    BRANCH_NAME="${BRANCH_PREFIX}/work"
fi
assert_eq "branch without plan step" "agent/general-agent-1/ticket-42/work" "${BRANCH_NAME}"

echo ""

# --- Test: repo extraction from description ---
echo "Repo extraction from description:"

extract_repo() {
    local desc="$1"
    echo "${desc}" | grep -oP '(?:repo|gitea)[:\s]+\K\S+/\S+' | head -1 || true
}

RESULT=$(extract_repo "Please work on repo: myorg/myrepo and fix the bug")
assert_eq "repo: owner/name format" "myorg/myrepo" "${RESULT}"

RESULT=$(extract_repo "Fix the bug in gitea: claude/dev-env immediately")
assert_eq "gitea: owner/name format" "claude/dev-env" "${RESULT}"

RESULT=$(extract_repo "Just fix the bug please")
assert_eq "no repo in description" "" "${RESULT}"

RESULT=$(extract_repo "repo:team/project-name is the target")
assert_eq "repo:owner/name no space" "team/project-name" "${RESULT}"

echo ""

# --- Test: required env vars ---
echo "Environment validation:"

REQUIRED_VARS=(
    TICKET_ID AGENT_ID AGENT_SPECIALIZATION
    GITEA_URL GITEA_USERNAME GITEA_PASSWORD
    TAIGA_URL TAIGA_USERNAME TAIGA_PASSWORD
)

assert_eq "required vars count" "9" "${#REQUIRED_VARS[@]}"
assert_contains "TICKET_ID in required" "TICKET_ID" "${REQUIRED_VARS[*]}"
assert_contains "TAIGA_PASSWORD in required" "TAIGA_PASSWORD" "${REQUIRED_VARS[*]}"

echo ""

# --- Test: system prompt exists ---
echo "Files:"

if [ -f "${SCRIPT_DIR}/system-prompt.md" ]; then
    echo "  [PASS] system-prompt.md exists"
    PASS=$((PASS + 1))
else
    echo "  [FAIL] system-prompt.md missing"
    FAIL=$((FAIL + 1))
fi

PROMPT_CONTENT=$(cat "${SCRIPT_DIR}/system-prompt.md")
assert_contains "system prompt has completion instructions" "completion-status.json" "${PROMPT_CONTENT}"
assert_contains "system prompt has code quality section" "magic constants" "${PROMPT_CONTENT}"
assert_contains "system prompt has identity section" "agent identity" "${PROMPT_CONTENT}"

echo ""

# --- Test: PR body generation ---
echo "PR body generation:"

TICKET_ID="42"
TICKET_SUBJECT="Fix login bug"
PLAN_STEP="2"
AGENT_ID="backend-agent-1"
COMPLETION_SUMMARY="Fixed the authentication flow"
COMPLETION_STATUS="success"
TAIGA_URL="http://localhost:9000"

PR_BODY="## Ticket
[#${TICKET_ID}: ${TICKET_SUBJECT}](${TAIGA_URL}/project/dev-environment/us/${TICKET_ID})
$([ -n "${PLAN_STEP}" ] && echo "
## Plan Step
Step ${PLAN_STEP}")

## Summary
${COMPLETION_SUMMARY:-Work by agent ${AGENT_ID}}

## Status
${COMPLETION_STATUS}"

assert_contains "PR body has ticket link" "#42: Fix login bug" "${PR_BODY}"
assert_contains "PR body has plan step" "Step 2" "${PR_BODY}"
assert_contains "PR body has summary" "Fixed the authentication flow" "${PR_BODY}"
assert_contains "PR body has Taiga URL" "localhost:9000" "${PR_BODY}"

echo ""

# --- Results ---
echo "---"
echo "Results: ${PASS} passed, ${FAIL} failed"

if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
