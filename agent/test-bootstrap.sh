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

    # Pure-bash substring test: piping a large haystack into "grep -q" makes
    # grep exit on the first match, which under "set -o pipefail" surfaces the
    # writer's SIGPIPE as a failed pipeline even though the needle was found.
    if [[ "${haystack}" == *"${needle}"* ]]; then
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

# Fix mode must adopt the PR's own head branch. An empty BRANCH_NAME here
# disables the checkout, the push and the no-commit guard at once, so the
# agent's work is computed and then discarded with the workspace.
MODE="fix"
PR_HEAD_REF="ticket-42/plan"
BRANCH_NAME="${PR_HEAD_REF}"
assert_eq "branch for fix mode is the PR head ref" "ticket-42/plan" "${BRANCH_NAME}"

BOOTSTRAP_SRC=$(cat "${SCRIPT_DIR}/bootstrap.sh")
assert_contains "fix mode assigns BRANCH_NAME from the PR head ref" \
    'BRANCH_NAME="${PR_HEAD_REF}"' "${BOOTSTRAP_SRC}"
assert_contains "PR metadata is fetched for fix as well as review" \
    'if [ "${MODE}" = "fix" ] || [ "${MODE}" = "review" ]; then' "${BOOTSTRAP_SRC}"
assert_contains "fix mode never invents a branch when the head ref is gone" \
    'no longer exists on origin' "${BOOTSTRAP_SRC}"
assert_contains "fix mode reports back on the PR" \
    '/issues/${PR_NUMBER}/comments' "${BOOTSTRAP_SRC}"

echo ""

# --- Test: repo extraction from description ---
echo "Repo extraction from description:"

# Replicate the extract_repo_ref function from bootstrap.sh
extract_repo_ref() {
    local desc="$1"
    local raw
    raw=$(echo "${desc}" | grep -oP '(?i)(?:repo|gitea)\s*:\s*\K.*' | head -1 || true)
    [ -z "${raw}" ] && return
    raw=$(printf '%s' "${raw}" | python3 -c '
import sys, unicodedata
s = sys.stdin.read()
def strippable(c):
    return c.isspace() or unicodedata.category(c) == "Cf"
i, j = 0, len(s)
while i < j and strippable(s[i]):
    i += 1
while j > i and strippable(s[j - 1]):
    j -= 1
sys.stdout.write(s[i:j])
')
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

# Invisible unicode codepoints (rich-text paste) — these arrive via Taiga's
# WYSIWYG editor and used to break the URL detection, routing REPO_REF to
# the owner/name branch which mangled it into REPO_OWNER=" https:" etc.
NBSP=$(printf '\xc2\xa0')
ZWSP=$(printf '\xe2\x80\x8b')
BOM=$(printf '\xef\xbb\xbf')
RESULT=$(extract_repo_ref "repo:${NBSP}https://github.com/org/repo")
assert_eq "repo: URL with leading NBSP" "https://github.com/org/repo" "${RESULT}"
RESULT=$(extract_repo_ref "repo:${ZWSP}https://github.com/org/repo")
assert_eq "repo: URL with leading ZWSP" "https://github.com/org/repo" "${RESULT}"
RESULT=$(extract_repo_ref "repo:${BOM}https://github.com/org/repo")
assert_eq "repo: URL with leading BOM" "https://github.com/org/repo" "${RESULT}"
RESULT=$(extract_repo_ref "repo: https://github.com/org/repo${ZWSP}")
assert_eq "repo: URL with trailing ZWSP" "https://github.com/org/repo" "${RESULT}"

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

# --- Test: review mode ---
echo "Review mode:"

if [ -f "${SCRIPT_DIR}/system-prompt-review.md" ]; then
    echo "  [PASS] system-prompt-review.md exists"
    PASS=$((PASS + 1))
else
    echo "  [FAIL] system-prompt-review.md missing"
    FAIL=$((FAIL + 1))
fi

REVIEW_PROMPT=$(cat "${SCRIPT_DIR}/system-prompt-review.md" 2>/dev/null || echo "")
assert_contains "review prompt forbids approving" "Never approve a PR" "${REVIEW_PROMPT}"
assert_contains "review prompt forbids request-changes" "REQUEST_CHANGES" "${REVIEW_PROMPT}"
assert_contains "review prompt forbids merging" "Never merge" "${REVIEW_PROMPT}"
assert_contains "review prompt names the result file" "review-result.json" "${REVIEW_PROMPT}"

# Every mode's system prompt must be baked into the image, or the mode
# silently runs on the base prompt alone.
for mode in analysis plan step fix onestep review; do
    if grep -qF "system-prompt-${mode}.md /home/agent/system-prompt-${mode}.md" "${SCRIPT_DIR}/Dockerfile"; then
        echo "  [PASS] Dockerfile copies system-prompt-${mode}.md"
        PASS=$((PASS + 1))
    else
        echo "  [FAIL] Dockerfile does not copy system-prompt-${mode}.md"
        FAIL=$((FAIL + 1))
    fi
done

# The reviewer must never be able to emit an approving or blocking
# review: the event is fixed in bootstrap.sh, not read from the agent.
BOOTSTRAP_SRC=$(cat "${SCRIPT_DIR}/bootstrap.sh")
assert_contains "review event hard-coded to COMMENT" 'REVIEW_EVENT="COMMENT"' "${BOOTSTRAP_SRC}"

if echo "${BOOTSTRAP_SRC}" | grep -qE 'REVIEW_EVENT=.*(APPROVE|REQUEST_CHANGES)'; then
    echo "  [FAIL] bootstrap can emit an approving/blocking review event"
    FAIL=$((FAIL + 1))
else
    echo "  [PASS] bootstrap never emits APPROVED or REQUEST_CHANGES"
    PASS=$((PASS + 1))
fi

# Review-mode branch naming: reviewers check out the PR head and own no branch.
MODE="review"
TICKET_ID="42"
BRANCH_PREFIX="ticket-${TICKET_ID}"
case "${MODE}" in
    analysis) BRANCH_NAME="" ;;
    plan)     BRANCH_NAME="${BRANCH_PREFIX}/plan" ;;
    review)   BRANCH_NAME="" ;;
    *)        BRANCH_NAME="${BRANCH_PREFIX}/step-1" ;;
esac
assert_eq "review mode creates no branch" "" "${BRANCH_NAME}"

# The SHA marker format is a contract shared with the orchestrator's
# reconciler (ReviewSHAMarker in pkg/reconciler). If these drift, every
# PR is reviewed again on every reconcile pass.
PR_HEAD_SHA="0a1b2c3d"
REVIEW_SHA_MARKER="<!-- agent-review: ${PR_HEAD_SHA} -->"
assert_eq "review SHA marker format" "<!-- agent-review: 0a1b2c3d -->" "${REVIEW_SHA_MARKER}"

GO_MARKER_SRC=$(cat "${SCRIPT_DIR}/../orchestrator/pkg/reconciler/reconciler.go" 2>/dev/null || echo "")
assert_contains "orchestrator uses the same marker prefix" '"<!-- agent-review: "' "${GO_MARKER_SRC}"
assert_contains "orchestrator uses the same marker suffix" '" -->"' "${GO_MARKER_SRC}"

echo ""

# --- Test: review payload construction ---
echo "Review payload:"

REVIEW_FIXTURE=$(mktemp)
cat > "${REVIEW_FIXTURE}" <<'REVIEW_JSON'
{
  "summary": "Adds a cache layer.",
  "verdict": "minor-issues",
  "comments": [
    {"path": "pkg/a.go", "line": 42, "severity": "blocker", "comment": "Indexes out of range."},
    {"path": "", "line": 5, "severity": "minor", "comment": "no path - dropped"},
    {"path": "pkg/c.go", "line": 0, "severity": "minor", "comment": "no line - dropped"}
  ]
}
REVIEW_JSON

build_review_payload() {
    local body="$1" event="$2" result_file="$3"
    jq -n --arg body "${body}" --arg event "${event}" --slurpfile result "${result_file}" \
        '(($result[0].comments // []) | map(select(
                (.path // "") != "" and ((.line // 0) | tonumber? // 0) > 0
            ) | {
                path: .path,
                body: (if (.severity // "") != "" then "**" + (.severity | ascii_upcase) + ":** " + (.comment // "") else (.comment // "") end),
                new_position: (.line | tonumber)
            })) as $inline
         | {body: $body, event: $event}
         | if ($inline | length) > 0 then . + {comments: $inline} else . end'
}

PAYLOAD=$(build_review_payload "Summary body" "COMMENT" "${REVIEW_FIXTURE}")
assert_eq "payload event is COMMENT" "COMMENT" "$(echo "${PAYLOAD}" | jq -r '.event')"
assert_eq "invalid inline comments dropped" "1" "$(echo "${PAYLOAD}" | jq '.comments | length')"
assert_eq "inline comment anchored to new side" "42" "$(echo "${PAYLOAD}" | jq -r '.comments[0].new_position')"
assert_contains "severity folded into comment body" "**BLOCKER:**" "$(echo "${PAYLOAD}" | jq -r '.comments[0].body')"

# A review with no usable inline comments must omit the key entirely —
# Gitea rejects an empty comments array on some versions.
EMPTY_FIXTURE=$(mktemp)
echo '{"summary":"Looks fine.","verdict":"looks-good"}' > "${EMPTY_FIXTURE}"
EMPTY_PAYLOAD=$(build_review_payload "Summary only" "COMMENT" "${EMPTY_FIXTURE}")
assert_eq "summary-only payload omits comments key" "null" "$(echo "${EMPTY_PAYLOAD}" | jq -r '.comments // "null"')"

rm -f "${REVIEW_FIXTURE}" "${EMPTY_FIXTURE}"

echo ""

# --- Test: fix-mode threaded replies ---
# Each inline review comment is answered in its own PR thread, addressed by
# the comment id the feedback section printed. Entries that name an id this
# PR does not carry are dropped: ids are repo-global, so a hallucinated one
# would otherwise land a reply on an unrelated PR.
echo "Fix-mode PR replies:"

# The prompt line must carry the id, or the agent has nothing to reply to.
REVIEW_COMMENTS_JSON='[{"id":501,"path":"pkg/a.go","new_position":42,"body":"Off by one."},{"id":502,"path":"pkg/b.go","new_position":null,"old_position":7,"body":"Dead code."}]'
RENDERED=$(echo "${REVIEW_COMMENTS_JSON}" \
    | jq -r '.[] | "- **\(.path):\(.new_position // .old_position // 0)** (comment_id: \(.id)) — \(.body)"')
assert_contains "inline comment renders its id" "(comment_id: 501)" "${RENDERED}"
assert_contains "inline comment falls back to old_position" "pkg/b.go:7" "${RENDERED}"
assert_eq "replyable ids collected" "501 502" "$(echo "${REVIEW_COMMENTS_JSON}" | jq -r '.[] | .id' | tr '\n' ' ' | sed 's/ $//')"

PR_REPLYABLE_COMMENT_IDS=" 501 502 "

# Mirrors the accept/drop decision in bootstrap.sh: an entry is posted only
# when it has an id this PR carries and a non-empty body.
reply_decision() {
    local entry="$1"
    local comment_id body
    comment_id=$(echo "${entry}" | jq -r 'if (.comment_id // null) == null then "" else (.comment_id | tostring) end')
    body=$(echo "${entry}" | jq -r '.reply // ""')
    if [ -z "${comment_id}" ] || [ -z "${body}" ]; then
        echo "dropped"
        return
    fi
    case " ${PR_REPLYABLE_COMMENT_IDS} " in
        *" ${comment_id} "*) echo "posted" ;;
        *) echo "dropped" ;;
    esac
}

# name | completion-status entry | expected decision
REPLY_CASES=(
    "known id with body|{\"comment_id\":501,\"reply\":\"Fixed.\"}|posted"
    "id given as string|{\"comment_id\":\"502\",\"reply\":\"Skipped: out of scope.\"}|posted"
    "unknown id|{\"comment_id\":999,\"reply\":\"Fixed.\"}|dropped"
    "id is a prefix of a known id|{\"comment_id\":50,\"reply\":\"Fixed.\"}|dropped"
    "missing id|{\"reply\":\"Fixed.\"}|dropped"
    "empty reply|{\"comment_id\":501,\"reply\":\"\"}|dropped"
)

for reply_case in "${REPLY_CASES[@]}"; do
    IFS='|' read -r case_name case_entry case_expected <<< "${reply_case}"
    assert_eq "reply ${case_name}" "${case_expected}" "$(reply_decision "${case_entry}")"
done

# A completion status without the key yields no replies at all, rather than
# an error that would take the whole post-processing block down.
REPLY_FIXTURE=$(mktemp)
echo '{"status":"success","summary":"Done."}' > "${REPLY_FIXTURE}"
assert_eq "absent pr_comment_replies yields nothing" "" \
    "$(jq -c '(.pr_comment_replies // [])[]' "${REPLY_FIXTURE}")"
echo '{"status":"success","pr_comment_replies":[{"comment_id":501,"reply":"Fixed."}]}' > "${REPLY_FIXTURE}"
assert_eq "present pr_comment_replies yields one entry" "1" \
    "$(jq -c '(.pr_comment_replies // [])[]' "${REPLY_FIXTURE}" | grep -c '')"
rm -f "${REPLY_FIXTURE}"

FIX_PROMPT_CONTENT=$(cat "${SCRIPT_DIR}/system-prompt-fix.md")
assert_contains "fix prompt documents pr_comment_replies" "pr_comment_replies" "${FIX_PROMPT_CONTENT}"
BOOTSTRAP_FIX_CONTENT=$(cat "${SCRIPT_DIR}/bootstrap.sh")
assert_contains "bootstrap posts to the replies endpoint" "/comments/\${REPLY_COMMENT_ID}/replies" "${BOOTSTRAP_FIX_CONTENT}"

echo ""

# --- Test: codebase-context file resolution ---
# AGENTS.md is the only name agents write; CLAUDE.md is accepted as a
# fallback for repos planned before the rename.
echo "Codebase context file:"

CONTEXT_FILE="AGENTS.md"
LEGACY_CONTEXT_FILE="CLAUDE.md"

resolve_context_md() {
    local workspace="$1"
    local context_md="${workspace}/${CONTEXT_FILE}"
    if [ ! -f "${context_md}" ]; then
        context_md="${workspace}/${LEGACY_CONTEXT_FILE}"
    fi
    if [ -f "${context_md}" ]; then
        basename "${context_md}"
    fi
}

# name | files present in the workspace | expected resolved file
CONTEXT_CASES=(
    "both present|AGENTS.md CLAUDE.md|AGENTS.md"
    "only AGENTS.md|AGENTS.md|AGENTS.md"
    "only legacy CLAUDE.md|CLAUDE.md|CLAUDE.md"
    "neither present||"
)

for context_case in "${CONTEXT_CASES[@]}"; do
    IFS='|' read -r case_name case_files case_expected <<< "${context_case}"
    CASE_WORKSPACE=$(mktemp -d)
    for case_file in ${case_files}; do
        echo "context" > "${CASE_WORKSPACE}/${case_file}"
    done
    assert_eq "context resolution: ${case_name}" "${case_expected}" "$(resolve_context_md "${CASE_WORKSPACE}")"
    rm -rf "${CASE_WORKSPACE}"
done

BOOTSTRAP_CONTENT=$(cat "${SCRIPT_DIR}/bootstrap.sh")
assert_contains "bootstrap uses AGENTS.md as context file" 'CONTEXT_FILE="AGENTS.md"' "${BOOTSTRAP_CONTENT}"
assert_contains "bootstrap keeps CLAUDE.md fallback" 'LEGACY_CONTEXT_FILE="CLAUDE.md"' "${BOOTSTRAP_CONTENT}"

PLAN_PROMPT_CONTENT=$(cat "${SCRIPT_DIR}/system-prompt-plan.md")
assert_contains "plan prompt writes AGENTS.md" 'Write `AGENTS.md` in the repo root' "${PLAN_PROMPT_CONTENT}"
assert_contains "plan prompt renames legacy CLAUDE.md" 'git mv CLAUDE.md AGENTS.md' "${PLAN_PROMPT_CONTENT}"

echo ""

# --- Test: progress checkpointing ---
#
# Mirrors push_progress / checkpoint_and_push from bootstrap.sh against a
# real repo and a real (file://) remote. The contract under test is the one
# that keeps a deadline-killed agent's work: committed work is pushed after
# every session, a dirty worktree is committed only when the pod is going
# away, and an agent that owns no branch pushes nothing.
echo "Progress checkpointing:"

PROGRESS_PUSH_TIMEOUT_SECONDS=60
WIP_COMMIT_SUBJECT="wip(agent): checkpoint uncommitted work"

push_progress() {
    local reason="$1"
    [ -n "${BRANCH_NAME}" ] || return 0
    cd "${WORKSPACE}" 2>/dev/null || return 0

    local local_head remote_head
    local_head=$(git rev-parse HEAD 2>/dev/null || echo "")
    [ -n "${local_head}" ] || return 0
    remote_head=$(git rev-parse "origin/${BRANCH_NAME}" 2>/dev/null || echo "")
    if [ "${local_head}" = "${remote_head}" ]; then
        return 0
    fi
    timeout "${PROGRESS_PUSH_TIMEOUT_SECONDS}" \
        git push -u origin "${BRANCH_NAME}" >/dev/null 2>&1 || true
    return 0
}

checkpoint_and_push() {
    local reason="$1"
    [ -n "${BRANCH_NAME}" ] || return 0
    cd "${WORKSPACE}" 2>/dev/null || return 0
    if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
        git add -A 2>/dev/null || true
        git commit -m "${WIP_COMMIT_SUBJECT}

Interrupted (${reason})." >/dev/null 2>&1 || true
    fi
    push_progress "${reason}"
}

# make_checkpoint_fixture <branch>
# Builds a bare remote plus a clone sitting on <branch> with one pushed
# commit, and echoes "<remote> <clone>".
make_checkpoint_fixture() {
    local branch="$1"
    local remote clone
    remote=$(mktemp -d)
    clone=$(mktemp -d)
    git init --quiet --bare "${remote}"
    git clone --quiet "${remote}" "${clone}" 2>/dev/null
    (
        cd "${clone}"
        git config user.email "agent@test.local"
        git config user.name "agent"
        git checkout --quiet -b "${branch}"
        echo "base" > base.txt
        git add base.txt
        git commit --quiet -m "base"
        git push --quiet -u origin "${branch}"
    )
    echo "${remote} ${clone}"
}

# remote_head_subject <remote> <branch>
remote_head_subject() {
    git --git-dir="$1" log -1 --format=%s "$2" 2>/dev/null || echo ""
}

# name | action under test | local change | expected remote HEAD subject
CHECKPOINT_CASES=(
    "committed work is pushed|push_progress|commit|agent work"
    "nothing new pushes nothing|push_progress|none|base"
    "dirty tree is not auto-committed|push_progress|dirty|base"
    "termination commits dirty tree|checkpoint_and_push|dirty|${WIP_COMMIT_SUBJECT}"
    "termination pushes committed work|checkpoint_and_push|commit|agent work"
)

for checkpoint_case in "${CHECKPOINT_CASES[@]}"; do
    IFS='|' read -r case_name case_fn case_change case_expected <<< "${checkpoint_case}"

    BRANCH_NAME="ticket-42/step-1"
    read -r CASE_REMOTE CASE_CLONE <<< "$(make_checkpoint_fixture "${BRANCH_NAME}")"
    WORKSPACE="${CASE_CLONE}"

    case "${case_change}" in
        commit)
            (cd "${CASE_CLONE}" && echo "work" > work.txt && git add work.txt \
                && git commit --quiet -m "agent work")
            ;;
        dirty)
            echo "uncommitted" > "${CASE_CLONE}/scratch.txt"
            ;;
    esac

    ( "${case_fn}" "test" ) >/dev/null 2>&1

    assert_eq "checkpoint: ${case_name}" "${case_expected}" \
        "$(remote_head_subject "${CASE_REMOTE}" "${BRANCH_NAME}")"
    rm -rf "${CASE_REMOTE}" "${CASE_CLONE}"
done

# An agent without a branch of its own (analysis, review) must never push.
BRANCH_NAME=""
read -r NOBRANCH_REMOTE NOBRANCH_CLONE <<< "$(make_checkpoint_fixture "ticket-42/step-1")"
WORKSPACE="${NOBRANCH_CLONE}"
echo "uncommitted" > "${NOBRANCH_CLONE}/scratch.txt"
( checkpoint_and_push "test" ) >/dev/null 2>&1
assert_eq "checkpoint: branchless agent pushes nothing" "base" \
    "$(remote_head_subject "${NOBRANCH_REMOTE}" "ticket-42/step-1")"
rm -rf "${NOBRANCH_REMOTE}" "${NOBRANCH_CLONE}"

assert_contains "bootstrap checkpoints after every session" \
    'push_progress "after session ${SESSION_NUM}"' "${BOOTSTRAP_CONTENT}"
assert_contains "bootstrap traps termination signals" \
    'trap on_termination TERM INT' "${BOOTSTRAP_CONTENT}"
assert_contains "bootstrap waits on a backgrounded Claude so the trap can run" \
    'wait "${CLAUDE_PID}"' "${BOOTSTRAP_CONTENT}"

echo ""

# --- Test: silent-failure guard ---
#
# The guard decides whether a run produced anything; "no" hands the ticket
# to a human and skips every post-processing step, including the fix
# agent's replies on the PR. It therefore has to stay correct in the
# presence of `push_progress`, which pushes the session's commits as a
# crash checkpoint before the guard ever runs. Comparing HEAD against
# `origin/${BRANCH_NAME}` did not: the checkpoint made the two equal and
# every successful fix run was reported as "produced no commits".
echo "Silent-failure guard:"

# compute_has_new_work
# Mirrors the HAS_NEW_WORK block in bootstrap.sh. Reads BRANCH_NAME, MODE,
# BRANCH_BASE_SHA and WORK_BRANCH from the environment; echoes true/false.
compute_has_new_work() {
    local has_new_work="false"
    if [ -n "${BRANCH_NAME}" ]; then
        if [ -n "$(git status --porcelain)" ]; then
            has_new_work="true"
        elif [ "${MODE}" = "fix" ]; then
            local local_head
            local_head=$(git rev-parse HEAD)
            if [ -n "${BRANCH_BASE_SHA}" ] && [ "${local_head}" != "${BRANCH_BASE_SHA}" ]; then
                has_new_work="true"
            fi
        else
            local commits_ahead
            commits_ahead=$(git rev-list --count "origin/${WORK_BRANCH}..HEAD" 2>/dev/null || echo 0)
            if [ "${commits_ahead}" != "0" ]; then
                has_new_work="true"
            fi
        fi
    fi
    echo "${has_new_work}"
}

# make_guard_fixture <branch>
# Builds a bare remote holding the work branch plus <branch> cut from it,
# and a clone sitting on <branch>. Echoes "<remote> <clone>".
make_guard_fixture() {
    local branch="$1"
    local remote clone
    remote=$(mktemp -d)
    clone=$(mktemp -d)
    git init --quiet --bare "${remote}"
    git clone --quiet "${remote}" "${clone}" 2>/dev/null
    (
        cd "${clone}"
        git config user.email "agent@test.local"
        git config user.name "agent"
        git checkout --quiet -b "ticket-42/work"
        echo "base" > base.txt
        git add base.txt
        git commit --quiet -m "base"
        git push --quiet -u origin "ticket-42/work"
        git checkout --quiet -b "${branch}"
        git push --quiet -u origin "${branch}"
    )
    echo "${remote} ${clone}"
}

WORK_BRANCH="ticket-42/work"

# name | mode | local change | checkpoint pushed first | expected HAS_NEW_WORK
GUARD_CASES=(
    "fix: unpushed commit counts|fix|commit|no|true"
    "fix: checkpointed commit still counts|fix|commit|yes|true"
    "fix: nothing done is reported honestly|fix|none|no|false"
    "fix: dirty worktree counts|fix|dirty|no|true"
    "step: commit ahead of work branch counts|step|commit|no|true"
    "step: checkpointed commit still counts|step|commit|yes|true"
    "step: nothing done is reported honestly|step|none|no|false"
)

for guard_case in "${GUARD_CASES[@]}"; do
    IFS='|' read -r case_name case_mode case_change case_checkpoint case_expected \
        <<< "${guard_case}"

    MODE="${case_mode}"
    BRANCH_NAME="ticket-42/step-1"
    read -r CASE_REMOTE CASE_CLONE <<< "$(make_guard_fixture "${BRANCH_NAME}")"
    WORKSPACE="${CASE_CLONE}"
    # Captured at checkout, exactly as bootstrap.sh does it.
    BRANCH_BASE_SHA=$(cd "${CASE_CLONE}" && git rev-parse HEAD)

    case "${case_change}" in
        commit)
            (cd "${CASE_CLONE}" && echo "work" > work.txt && git add work.txt \
                && git commit --quiet -m "agent work")
            ;;
        dirty)
            echo "uncommitted" > "${CASE_CLONE}/scratch.txt"
            ;;
    esac

    if [ "${case_checkpoint}" = "yes" ]; then
        ( push_progress "after session 1" ) >/dev/null 2>&1
    fi

    assert_eq "guard: ${case_name}" "${case_expected}" \
        "$(cd "${CASE_CLONE}" && compute_has_new_work)"
    rm -rf "${CASE_REMOTE}" "${CASE_CLONE}"
done

assert_contains "guard compares fix work against the checkout baseline" \
    '[ "${LOCAL_HEAD}" != "${BRANCH_BASE_SHA}" ]' "${BOOTSTRAP_CONTENT}"

# The baseline is only a baseline if it is read before the first
# checkpoint push can move the branch.
GUARD_BASE_LINE=$(grep -n '^BRANCH_BASE_SHA=""' "${SCRIPT_DIR}/bootstrap.sh" | cut -d: -f1)
GUARD_PUSH_LINE=$(grep -n 'push_progress "after session' "${SCRIPT_DIR}/bootstrap.sh" | cut -d: -f1)
if [ -n "${GUARD_BASE_LINE}" ] && [ -n "${GUARD_PUSH_LINE}" ] \
        && [ "${GUARD_BASE_LINE}" -lt "${GUARD_PUSH_LINE}" ]; then
    GUARD_ORDER="before"
else
    GUARD_ORDER="after"
fi
assert_eq "baseline is captured before the first checkpoint push" "before" "${GUARD_ORDER}"

echo ""

# --- Test: Claude CLI invocation flags ---
#
# Model and effort are both optional: an orchestrator that predates either
# variable must still produce a working invocation, and an empty value must
# never reach the CLI as a bare "--model"/"--effort" with nothing after it.
echo "CLI flags:"

# build_claude_args
# Mirrors the CLAUDE_ARGS assembly in bootstrap.sh. Echoes the flags it adds.
build_claude_args() {
    local args=()
    if [ -n "${CLAUDE_MODEL:-}" ]; then
        args+=(--model "${CLAUDE_MODEL}")
    fi
    if [ -n "${CLAUDE_EFFORT:-}" ]; then
        args+=(--effort "${CLAUDE_EFFORT}")
    fi
    echo "${args[*]:-}"
}

# name | CLAUDE_MODEL | CLAUDE_EFFORT | expected added flags
CLI_FLAG_CASES=(
    "model and effort both set|claude-opus-5-5|high|--model claude-opus-5-5 --effort high"
    "effort unset is omitted entirely|claude-opus-5-5||--model claude-opus-5-5"
    "model unset is omitted entirely||high|--effort high"
    "neither set adds no flags|||"
)

for cli_case in "${CLI_FLAG_CASES[@]}"; do
    IFS='|' read -r case_name case_model case_effort case_expected <<< "${cli_case}"
    CLAUDE_MODEL="${case_model}" CLAUDE_EFFORT="${case_effort}"
    assert_eq "cli: ${case_name}" "${case_expected}" "$(build_claude_args)"
done
unset CLAUDE_MODEL CLAUDE_EFFORT

assert_contains "bootstrap passes the effort level to the CLI" \
    'CLAUDE_ARGS+=(--effort "${CLAUDE_EFFORT}")' "${BOOTSTRAP_CONTENT}"
assert_contains "bootstrap guards the effort flag on a non-empty value" \
    'if [ -n "${CLAUDE_EFFORT:-}" ]; then' "${BOOTSTRAP_CONTENT}"

echo ""

# --- Results ---
echo "---"
echo "Results: ${PASS} passed, ${FAIL} failed"

if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
