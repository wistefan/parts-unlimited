#!/usr/bin/env python3
"""PreToolUse hook that caps Bash tool-result size.

Claude Code's `Bash` builtin returns the full stdout/stderr of whatever
command the agent runs. For commands like `go test ./...`, `helm lint`,
`./eval.sh`, or a `Read` of a 2k-line file, that output can be 10-200 KB.
Once it lands in the conversation it is cached and re-paid on EVERY
subsequent assistant turn in that session — empirically ~60K cache-read
tokens per turn × 20+ turns = 1M+ wasted tokens per step where most of
the re-paid content was noise (passing tests, duplicate log lines, etc.).

This hook intercepts the Bash call BEFORE it runs, executes the command
itself (in the same cwd and env), truncates the output head+tail with
an elision marker, then `deny`s the builtin invocation and presents the
truncated result via `permissionDecisionReason`. The model sees the
short version; the builtin never fires so there is no double execution.

Why head+tail: for logs/test output the first N lines give startup +
context and the last M lines carry the verdict (PASS / FAIL line,
error stack, final line count). The middle is usually repetitive
progress and can be dropped without loss of signal. The marker tells
the agent the elision happened and suggests narrower filters
(grep/head/tail) for a deeper look.
"""

import json
import os
import subprocess
import sys

# Truncation thresholds.
#
# CHAR_THRESHOLD: below this, return output untouched — small results
# are not worth the truncation ceremony and the marker itself would be
# a larger fraction of the content than the savings.
#
# HEAD/TAIL_LINES: kept at head and tail when line count exceeds
# HEAD+TAIL+SMALL_TAIL_ROOM. 60+30 = 90 lines keeps the "interesting
# stuff" at both ends of typical build/test output without bloating
# context.
#
# DEFAULT_TIMEOUT_SEC: fallback when the Bash tool input does not pin
# a timeout. Matches the Claude Code Bash default (2 minutes).
CHAR_THRESHOLD = 8000
HEAD_LINES = 60
TAIL_LINES = 30
SMALL_TAIL_ROOM = 5
DEFAULT_TIMEOUT_SEC = 120


def truncate(text: str) -> str:
    if len(text) <= CHAR_THRESHOLD:
        return text
    lines = text.splitlines()
    if len(lines) > (HEAD_LINES + TAIL_LINES + SMALL_TAIL_ROOM):
        elided = len(lines) - HEAD_LINES - TAIL_LINES
        marker = (
            f"... [{elided} lines truncated by bootstrap hook. Full output "
            f"was {len(text)} chars, {len(lines)} lines. Use narrower "
            f"filters (grep / head / tail / awk) if you need the middle.] ..."
        )
        kept = lines[:HEAD_LINES] + [marker] + lines[-TAIL_LINES:]
        return "\n".join(kept)
    # Few long lines (e.g. single-line JSON blob): fall back to char-level.
    half = CHAR_THRESHOLD // 2
    elided_chars = len(text) - CHAR_THRESHOLD
    return (
        text[:half]
        + f"\n... [middle char-truncated: {elided_chars} chars elided by bootstrap hook] ...\n"
        + text[-half:]
    )


def _resolve_timeout(tool_input: dict) -> int:
    """Bash tool's `timeout` is in milliseconds; fall back when absent."""
    raw = tool_input.get("timeout")
    if isinstance(raw, int) and raw > 0:
        return max(1, raw // 1000)
    return DEFAULT_TIMEOUT_SEC


def main() -> None:
    try:
        data = json.load(sys.stdin)
    except json.JSONDecodeError:
        sys.exit(0)  # never wedge on a malformed event

    if data.get("tool_name") != "Bash":
        sys.exit(0)

    tool_input = data.get("tool_input") or {}
    cmd = tool_input.get("command") or ""
    if not cmd.strip():
        sys.exit(0)

    # Background commands return immediately with an internal job handle;
    # their output is fetched later via Monitor. Replacing that lifecycle
    # would break the agent's async polling — let the builtin handle them.
    if tool_input.get("run_in_background"):
        sys.exit(0)

    timeout_sec = _resolve_timeout(tool_input)
    cwd = data.get("cwd") or os.getcwd()

    try:
        proc = subprocess.run(
            ["bash", "-c", cmd],
            cwd=cwd,
            capture_output=True,
            text=True,
            timeout=timeout_sec,
            env=os.environ.copy(),
        )
        stdout = proc.stdout or ""
        stderr = proc.stderr or ""
        if stdout and stderr:
            combined = f"{stdout}\n[stderr]\n{stderr}"
        else:
            combined = stdout or (f"[stderr]\n{stderr}" if stderr else "")
        exit_code = proc.returncode
    except subprocess.TimeoutExpired as exc:
        combined = (
            f"[command timed out after {timeout_sec}s]\n"
            f"{exc.stdout or ''}\n"
            f"[stderr]\n{exc.stderr or ''}"
        )
        exit_code = 124
    except Exception as exc:  # pragma: no cover - defensive
        combined = f"[hook error: {exc}]"
        exit_code = 1

    truncated = truncate(combined).strip() or "(no output)"

    response = {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "deny",
            "permissionDecisionReason": f"[exit={exit_code}]\n{truncated}",
        }
    }
    json.dump(response, sys.stdout)


if __name__ == "__main__":
    main()
