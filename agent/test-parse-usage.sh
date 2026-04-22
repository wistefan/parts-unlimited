#!/usr/bin/env bash
# Unit tests for parse-usage.py.
# Builds synthetic stream-json fixtures and asserts on both markdown and
# Prometheus outputs. Runs without a Claude invocation.

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

PROM=$(python3 "${PARSER}" "${FIXTURE_1}" --format prometheus --duration 12.5)
assert_contains "prom: input total metric"        "agent_tokens_input_total 120" "${PROM}"
assert_contains "prom: output total metric"       "agent_tokens_output_total 80" "${PROM}"
assert_contains "prom: cache write total metric"  "agent_tokens_cache_write_total 400" "${PROM}"
assert_contains "prom: cache read total metric"   "agent_tokens_cache_read_total 480" "${PROM}"
assert_contains "prom: turns total"               "agent_turns_total 2" "${PROM}"
assert_contains "prom: sessions total"            "agent_sessions_total 1" "${PROM}"
assert_contains "prom: duration seconds"          "agent_duration_seconds 12.5" "${PROM}"

# Per-session
assert_contains "prom: session 1 input"           'agent_session_tokens_input{session="1"} 120' "${PROM}"
assert_contains "prom: session 1 cache hit ratio" 'agent_session_cache_hit_ratio{session="1"} 0.8' "${PROM}"

# Per-turn
assert_contains "prom: turn 1 input"              'agent_turn_tokens_input{session="1",turn="1"} 100' "${PROM}"
assert_contains "prom: turn 2 cache read"         'agent_turn_tokens_cache_read{session="1",turn="2"} 480' "${PROM}"

# Tool attribution: Read in turn 1, Bash in turn 2. agent_tool_tokens_after
# for the Read call should be turn 2's input (20) + cache_read (480) = 500.
assert_contains "prom: Read tool call in turn 1"  'agent_tool_calls_total{session="1",turn="1",tool="Read"} 1' "${PROM}"
assert_contains "prom: Read tokens_after=500"     'agent_tool_tokens_after{session="1",turn="1",tool="Read"} 500' "${PROM}"
assert_contains "prom: Bash tool call in turn 2"  'agent_tool_calls_total{session="1",turn="2",tool="Bash"} 1' "${PROM}"
# No next turn for Bash, so tokens_after should be 0
assert_contains "prom: Bash tokens_after=0"       'agent_tool_tokens_after{session="1",turn="2",tool="Bash"} 0' "${PROM}"

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

PROM2=$(python3 "${PARSER}" "${FIXTURE_2}" --format prometheus)
assert_contains "prom: sessions=2"           "agent_sessions_total 2" "${PROM2}"
assert_contains "prom: session 1 input=10"   'agent_session_tokens_input{session="1"} 10' "${PROM2}"
assert_contains "prom: session 2 input=7"    'agent_session_tokens_input{session="2"} 7' "${PROM2}"
# Each session restarts its own turn counter.
assert_contains "prom: s1 turn 1"            'agent_turn_tokens_input{session="1",turn="1"} 10' "${PROM2}"
assert_contains "prom: s2 turn 1"            'agent_turn_tokens_input{session="2",turn="1"} 7' "${PROM2}"

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

PROM3=$(python3 "${PARSER}" "${FIXTURE_3}" --format prometheus)
assert_contains "prom: malformed line skipped, session=1" "agent_sessions_total 1" "${PROM3}"
# Read called twice in the same turn → single label set with count=2
assert_contains "prom: duplicate tool count aggregated"   'agent_tool_calls_total{session="1",turn="1",tool="Read"} 2' "${PROM3}"
# Should not produce two separate lines for the same tool/turn
DUP_COUNT=$(printf '%s\n' "${PROM3}" | grep -c 'agent_tool_calls_total{session="1",turn="1",tool="Read"}' || true)
assert_eq "prom: only one line for the aggregated tool" "1" "${DUP_COUNT}"

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

PROM4=$(python3 "${PARSER}" "${FIXTURE_4}" --format prometheus)
assert_contains "prom: empty still emits job gauges"     "agent_tokens_input_total 0" "${PROM4}"
assert_not_contains "prom: no per-session metrics"       "agent_session_tokens_input{" "${PROM4}"
assert_not_contains "prom: no tool metrics"              "agent_tool_calls_total{" "${PROM4}"

echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
[ "${FAIL}" -eq 0 ]
