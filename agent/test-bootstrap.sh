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

TICKET_ID="42"
BRANCH_PREFIX="ticket-${TICKET_ID}"

PLAN_STEP="3"
BRANCH_NAME="${BRANCH_PREFIX}/step-${PLAN_STEP}"
assert_eq "branch with plan step" "ticket-42/step-3" "${BRANCH_NAME}"

PLAN_STEP=""
if [ -n "${PLAN_STEP}" ]; then
    BRANCH_NAME="${BRANCH_PREFIX}/step-${PLAN_STEP}"
else
    BRANCH_NAME="${BRANCH_PREFIX}/work"
fi
assert_eq "branch without plan step" "ticket-42/work" "${BRANCH_NAME}"

MODE="plan"
BRANCH_NAME="${BRANCH_PREFIX}/plan"
assert_eq "branch for plan mode" "ticket-42/plan" "${BRANCH_NAME}"

echo ""

# --- Test: repo extraction from description ---
echo "Repo extraction from description:"

# Replicate the extract_repo_ref function from bootstrap.sh
extract_repo_ref() {
    local desc="$1"
    local raw
    raw=$(echo "${desc}" | grep -oP '(?i)(?:repo|gitea)\s*:\s*\K.*' | head -1 || true)
    [ -z "${raw}" ] && return
    raw=$(echo "${raw}" | sed -E 's/^\s+//;s/\s+$//')
    if echo "${raw}" | grep -qP '^\[.*\]\(.*\)'; then
        raw=$(echo "${raw}" | sed -E 's/^\[([^]]*)\]\(([^)]*)\).*/\2/')
    fi
    raw=$(echo "${raw}" | sed -E 's/^\[([^]]*)\]$/\1/')
    raw=$(echo "${raw}" | sed 's/^<//;s/>$//')
    raw=$(echo "${raw}" | awk '{print $1}')
    echo "${raw}"
}

RESULT=$(extract_repo_ref "Please work on repo: myorg/myrepo and fix the bug")
assert_eq "repo: owner/name format" "myorg/myrepo" "${RESULT}"

RESULT=$(extract_repo_ref "Fix the bug in gitea: claude/dev-env immediately")
assert_eq "gitea: owner/name format" "claude/dev-env" "${RESULT}"

RESULT=$(extract_repo_ref "Just fix the bug please")
assert_eq "no repo in description" "" "${RESULT}"

RESULT=$(extract_repo_ref "repo:team/project-name is the target")
assert_eq "repo:owner/name no space" "team/project-name" "${RESULT}"

RESULT=$(extract_repo_ref "repo: https://github.com/FIWARE/data-space-connector")
assert_eq "repo: full https URL" "https://github.com/FIWARE/data-space-connector" "${RESULT}"

RESULT=$(extract_repo_ref "repo: [https://github.com/org/repo](https://github.com/org/repo)")
assert_eq "repo: markdown link URL" "https://github.com/org/repo" "${RESULT}"

RESULT=$(extract_repo_ref "repo: <https://github.com/org/repo>")
assert_eq "repo: angle bracket URL" "https://github.com/org/repo" "${RESULT}"

RESULT=$(extract_repo_ref "repo: https://github.com/org/repo.git")
assert_eq "repo: URL with .git suffix" "https://github.com/org/repo.git" "${RESULT}"

RESULT=$(extract_repo_ref "Repo: claude/my-project")
assert_eq "repo: case insensitive" "claude/my-project" "${RESULT}"

RESULT=$(extract_repo_ref "repo: owner/name # this is a comment")
assert_eq "repo: value with trailing comment" "owner/name" "${RESULT}"

# Test that URL is detected as remote (not split into owner/name)
RESULT=$(extract_repo_ref "repo: https://github.com/FIWARE/data-space-connector")
if echo "${RESULT}" | grep -qP '^https?://'; then
    echo "  [PASS] URL detected as remote"
    PASS=$((PASS + 1))
else
    echo "  [FAIL] URL detected as remote"
    echo "         got: ${RESULT}"
    FAIL=$((FAIL + 1))
fi

# Test repo name extraction from URL
CLEAN_URL="${RESULT%.git}"
CLEAN_URL="${CLEAN_URL%/}"
REPO_NAME_FROM_URL=$(basename "${CLEAN_URL}")
assert_eq "repo name from URL" "data-space-connector" "${REPO_NAME_FROM_URL}"

echo ""

# --- Test: base branch extraction from description ---
echo "Base branch extraction:"

extract_base_branch() {
    local desc="$1"
    local raw
    raw=$(echo "${desc}" | grep -oP '(?i)base\s*:\s*\K\S+' | head -1 || true)
    echo "${raw}"
}

RESULT=$(extract_base_branch "repo: claude/test\nbase: develop")
assert_eq "base: develop" "develop" "${RESULT}"

RESULT=$(extract_base_branch "repo: claude/test")
assert_eq "base: not specified" "" "${RESULT}"

RESULT=$(extract_base_branch "Base: release/v2")
assert_eq "base: case insensitive" "release/v2" "${RESULT}"

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

# --- Test: implementation-plan slicing ---
echo "Plan slicing:"

# Mirrors the python3 invocation in bootstrap.sh that trims the plan down to
# the current step before embedding it in a step-mode task prompt.
slice_plan_for_step() {
    local step="$1"
    local plan="$2"
    PLAN_STEP_NUM="${step}" python3 -c '
import os, re, sys
raw = sys.stdin.read()
step = os.environ["PLAN_STEP_NUM"]
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
    sys.stdout.write(raw)
else:
    sys.stdout.write("".join(out))
    sys.stdout.write(
        "\n_Note: only step " + step + " is shown; "
        "read IMPLEMENTATION_PLAN.md in the workspace root for prior/later "
        "steps if needed._\n"
    )
' <<< "${plan}"
}

PLAN='# Implementation Plan: Test

## Overview
Some overview.

## Steps

### Step 1: First
First body.

### Step 2: Second
Second body line 1.
Second body line 2.

### Step 3: Third
Third body.'

SLICED=$(slice_plan_for_step "2" "${PLAN}")
assert_contains "slice step 2 keeps preamble" "Some overview" "${SLICED}"
assert_contains "slice step 2 keeps step 2 heading" "### Step 2: Second" "${SLICED}"
assert_contains "slice step 2 keeps step 2 body" "Second body line 2" "${SLICED}"
if echo "${SLICED}" | grep -q "### Step 1:"; then
    echo "  [FAIL] slice step 2 excludes step 1 heading"
    FAIL=$((FAIL + 1))
else
    echo "  [PASS] slice step 2 excludes step 1 heading"
    PASS=$((PASS + 1))
fi
if echo "${SLICED}" | grep -q "### Step 3:"; then
    echo "  [FAIL] slice step 2 excludes step 3 heading"
    FAIL=$((FAIL + 1))
else
    echo "  [PASS] slice step 2 excludes step 3 heading"
    PASS=$((PASS + 1))
fi
assert_contains "slice step 2 has note" "only step 2 is shown" "${SLICED}"

SLICED=$(slice_plan_for_step "1" "${PLAN}")
assert_contains "slice step 1 keeps step 1 body" "First body" "${SLICED}"
if echo "${SLICED}" | grep -q "### Step 2:"; then
    echo "  [FAIL] slice step 1 excludes step 2 heading"
    FAIL=$((FAIL + 1))
else
    echo "  [PASS] slice step 1 excludes step 2 heading"
    PASS=$((PASS + 1))
fi

# Unknown step number falls back to the full plan rather than emitting
# an empty prompt section.
SLICED=$(slice_plan_for_step "99" "${PLAN}")
assert_contains "slice unknown step falls back to full plan" "### Step 1: First" "${SLICED}"
assert_contains "slice unknown step has step 3" "### Step 3: Third" "${SLICED}"

echo ""

# --- Results ---
echo "---"
echo "Results: ${PASS} passed, ${FAIL} failed"

if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
