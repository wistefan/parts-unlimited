#!/usr/bin/env bash
# Unit tests for parse-usage.py.
# Builds synthetic stream-json fixtures and asserts on markdown output.
# Runs without a Claude invocation.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PARSER="${SCRIPT_DIR}/parse-usage.py"

PASS=0
FAIL=0
TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

assert_contains() {
    local name="$1"
    local needle="$2"
    local haystack="$3"

    if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        echo "  [PASS] ${name}"
        PASS=$((PASS + 1))
    else
        echo "  [FAIL] ${name}"
        echo "         expected to contain: ${needle}"
        echo "         actual:"
        printf '%s\n' "${haystack}" | sed 's/^/           /'
        FAIL=$((FAIL + 1))
    fi
}

assert_not_contains() {
    local name="$1"
    local needle="$2"
    local haystack="$3"

    if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        echo "  [FAIL] ${name}"
        echo "         did not expect: ${needle}"
        FAIL=$((FAIL + 1))
    else
        echo "  [PASS] ${name}"
        PASS=$((PASS + 1))
    fi
}

assert_eq() {
    local name="$1"
    local expected="$2"
    local actual="$3"

    if [ "${expected}" = "${actual}" ]; then
        echo "  [PASS] ${name}"
        PASS=$((PASS + 1))
    else
        echo "  [FAIL] ${name}"
        echo "         expected: ${expected}"
        echo "         actual:   ${actual}"
        FAIL=$((FAIL + 1))
    fi
}

echo "=== parse-usage.py tests ==="
echo ""

# ---------------------------------------------------------------------------
# Fixture 1: single session, two turns, one tool call per turn
# ---------------------------------------------------------------------------
FIXTURE_1="${TMPDIR}/single-session.jsonl"
cat > "${FIXTURE_1}" <<'EOF'
{"type":"system","subtype":"init","session_id":"s1"}
{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":400,"cache_read_input_tokens":0},"content":[{"type":"tool_use","name":"Read","input":{"file":"x"}}]}}
{"type":"user","message":{"role":"user","content":"tool_result"}}
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":20,"output_tokens":30,"cache_creation_input_tokens":0,"cache_read_input_tokens":480},"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
EOF

echo "Single session, two turns:"

OUT=$(python3 "${PARSER}" "${FIXTURE_1}" --format markdown)
# Totals: input=120, output=80, cache_write=400, cache_read=480, turns=2
assert_contains "markdown: input total"       "120" "${OUT}"
assert_contains "markdown: output total"      "80" "${OUT}"
assert_contains "markdown: cache write total" "400" "${OUT}"
assert_contains "markdown: cache read total"  "480" "${OUT}"
assert_contains "markdown: turn count"        "| Turns | 2 |" "${OUT}"
assert_contains "markdown: session count"     "| Sessions | 1 |" "${OUT}"
assert_contains "markdown: has total row"     "| **Total** |" "${OUT}"

echo ""

# ---------------------------------------------------------------------------
# Fixture 2: two sessions (two init events)
# ---------------------------------------------------------------------------
FIXTURE_2="${TMPDIR}/two-sessions.jsonl"
cat > "${FIXTURE_2}" <<'EOF'
{"type":"system","subtype":"init","session_id":"a"}
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":0},"content":[]}}
{"type":"system","subtype":"init","session_id":"b"}
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":90},"content":[]}}
EOF

echo "Two sessions:"

OUT2=$(python3 "${PARSER}" "${FIXTURE_2}" --format markdown)
assert_contains "markdown: two-session input total"  "17" "${OUT2}"
assert_contains "markdown: two-session output total" "8" "${OUT2}"
assert_contains "markdown: two-session sessions"     "| Sessions | 2 |" "${OUT2}"

echo ""

# ---------------------------------------------------------------------------
# Fixture 3: malformed line + same tool called twice in one turn
# ---------------------------------------------------------------------------
FIXTURE_3="${TMPDIR}/edge-cases.jsonl"
cat > "${FIXTURE_3}" <<'EOF'
not-json-at-all
{"type":"system","subtype":"init"}
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[{"type":"tool_use","name":"Read"},{"type":"text","text":"thinking"},{"type":"tool_use","name":"Read"}]}}
EOF

echo "Edge cases:"

OUT3=$(python3 "${PARSER}" "${FIXTURE_3}" --format markdown)
assert_contains "markdown: malformed line skipped, sessions=1" "| Sessions | 1 |" "${OUT3}"

echo ""

# ---------------------------------------------------------------------------
# Fixture 4: empty input
# ---------------------------------------------------------------------------
FIXTURE_4="${TMPDIR}/empty.jsonl"
: > "${FIXTURE_4}"

echo "Empty input:"

OUT4=$(python3 "${PARSER}" "${FIXTURE_4}" --format markdown)
assert_contains "markdown: empty still renders header"   "| Metric | Tokens |" "${OUT4}"
assert_contains "markdown: empty has zero turns"         "| Turns | 0 |" "${OUT4}"
assert_contains "markdown: empty has zero sessions"      "| Sessions | 0 |" "${OUT4}"

echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
[ "${FAIL}" -eq 0 ]
