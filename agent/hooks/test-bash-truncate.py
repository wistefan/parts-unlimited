#!/usr/bin/env python3
"""Unit tests for the bash-truncate PreToolUse hook.

Tests the pure truncate() function plus the JSON wire protocol end-to-end
by invoking the hook as a subprocess with crafted stdin payloads.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
HOOK = os.path.join(HERE, "bash-truncate.py")

sys.path.insert(0, HERE)
truncate_mod = __import__("bash-truncate".replace("-", "_"), fromlist=["truncate"]) \
    if False else None  # dash in filename — load via runpy below

import runpy  # noqa: E402
_hook_ns = runpy.run_path(HOOK)
truncate = _hook_ns["truncate"]
CHAR_THRESHOLD = _hook_ns["CHAR_THRESHOLD"]
HEAD_LINES = _hook_ns["HEAD_LINES"]
TAIL_LINES = _hook_ns["TAIL_LINES"]


class TestTruncate(unittest.TestCase):
    def test_below_threshold_passthrough(self):
        text = "a" * (CHAR_THRESHOLD - 1)
        self.assertEqual(truncate(text), text)

    def test_exactly_at_threshold_passthrough(self):
        text = "a" * CHAR_THRESHOLD
        self.assertEqual(truncate(text), text)

    def test_many_lines_head_tail_elision(self):
        total = HEAD_LINES + TAIL_LINES + 500
        lines = [f"line-{i:04d} " + "x" * 40 for i in range(total)]
        text = "\n".join(lines)
        out = truncate(text)
        # Head preserved
        self.assertIn(f"line-{0:04d}", out)
        self.assertIn(f"line-{HEAD_LINES - 1:04d}", out)
        # Tail preserved
        self.assertIn(f"line-{total - 1:04d}", out)
        self.assertIn(f"line-{total - TAIL_LINES:04d}", out)
        # Middle elided
        self.assertNotIn(f"line-{HEAD_LINES + 10:04d}", out)
        # Marker present and mentions the numbers
        self.assertIn("truncated by bootstrap hook", out)
        expected_elided = total - HEAD_LINES - TAIL_LINES
        self.assertIn(str(expected_elided), out)
        # Result substantially smaller than input
        self.assertLess(len(out), len(text) // 4)

    def test_few_long_lines_falls_back_to_char_truncation(self):
        # One massive single-line blob (e.g., JSON with no newlines)
        text = "A" * 2000 + "B" * 10000 + "C" * 2000
        out = truncate(text)
        self.assertIn("char-truncated", out)
        # Head "AAAA..." preserved
        self.assertTrue(out.startswith("A"))
        # Tail "CCC..." preserved
        self.assertTrue(out.endswith("C"))
        self.assertLess(len(out), len(text))


class TestHookWireProtocol(unittest.TestCase):
    """End-to-end: invoke the hook as a subprocess, feed a JSON event,
    parse its JSON response, verify shape."""

    def _run(self, event: dict) -> dict:
        proc = subprocess.run(
            [sys.executable, HOOK],
            input=json.dumps(event),
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        if proc.returncode != 0 and not proc.stdout:
            self.fail(f"hook exited {proc.returncode} with no stdout; stderr={proc.stderr}")
        if not proc.stdout.strip():
            return {}  # hook passed through (sys.exit(0))
        return json.loads(proc.stdout)

    def test_non_bash_tool_is_noop(self):
        out = self._run({"tool_name": "Read", "tool_input": {"file_path": "/tmp/x"}})
        self.assertEqual(out, {})

    def test_empty_command_is_noop(self):
        out = self._run({"tool_name": "Bash", "tool_input": {"command": "   "}})
        self.assertEqual(out, {})

    def test_background_command_is_noop(self):
        out = self._run({
            "tool_name": "Bash",
            "tool_input": {"command": "sleep 10", "run_in_background": True},
        })
        self.assertEqual(out, {})

    def test_small_output_is_untruncated(self):
        out = self._run({
            "tool_name": "Bash",
            "tool_input": {"command": "echo hello-small"},
            "cwd": "/tmp",
        })
        reason = out["hookSpecificOutput"]["permissionDecisionReason"]
        self.assertIn("hello-small", reason)
        self.assertNotIn("truncated by bootstrap hook", reason)
        self.assertIn("exit=0", reason)

    def test_large_output_is_truncated(self):
        with tempfile.TemporaryDirectory() as tmp:
            cmd = 'for i in $(seq 1 3000); do echo "line-$i content content content content content"; done'
            out = self._run({
                "tool_name": "Bash",
                "tool_input": {"command": cmd},
                "cwd": tmp,
            })
            reason = out["hookSpecificOutput"]["permissionDecisionReason"]
            self.assertIn("truncated by bootstrap hook", reason)
            # Head preserved: line-1 through line-60-ish
            self.assertIn("line-1 ", reason)
            # Tail preserved: line-3000
            self.assertIn("line-3000 ", reason)
            # Middle elided: line-1500 should be gone
            self.assertNotIn("line-1500 ", reason)
            # Far smaller than the raw ~170 KB output
            self.assertLess(len(reason), 20000)

    def test_decision_field_is_deny(self):
        out = self._run({
            "tool_name": "Bash",
            "tool_input": {"command": "echo x"},
            "cwd": "/tmp",
        })
        self.assertEqual(out["hookSpecificOutput"]["hookEventName"], "PreToolUse")
        self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")

    def test_nonzero_exit_code_propagated(self):
        out = self._run({
            "tool_name": "Bash",
            "tool_input": {"command": "exit 7"},
            "cwd": "/tmp",
        })
        reason = out["hookSpecificOutput"]["permissionDecisionReason"]
        self.assertIn("exit=7", reason)

    def test_stderr_captured(self):
        out = self._run({
            "tool_name": "Bash",
            "tool_input": {"command": "echo to-stderr 1>&2"},
            "cwd": "/tmp",
        })
        reason = out["hookSpecificOutput"]["permissionDecisionReason"]
        self.assertIn("to-stderr", reason)
        self.assertIn("[stderr]", reason)


if __name__ == "__main__":
    unittest.main(verbosity=2)
