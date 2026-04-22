#!/usr/bin/env python3
"""Build a compact textual digest of a Claude Code stream-json session.

The bootstrap loop calls this when an agent session ends without writing
a `completion-status.json` (typically because Claude hit the per-session
max-turns limit). The digest is fed to a separate `claude -p` invocation
that produces a `### Context Summary` block which then becomes the
prepended context for the next session.

Why a separate digest step instead of feeding the raw stream-json to
Claude:

* Stream-json carries the entire system prompt, tool_use envelopes,
  and tool_result blobs — most of which are noise for summarization.
  Sending the raw file would re-pay tokens for everything Claude already
  saw and burn the prompt cache for the next real session.
* The digest collapses each assistant turn into its text + tool call
  names, so the summarizer prompt stays under ~3K tokens even for a
  full 50-turn session.

The output is plain text (not JSON) shaped like:

    Turn 1
      tools: Read, Glob
      text: "Looking at the codebase to find ..."

    Turn 2
      tools: Edit
      text: "Updated handler to do X..."

The "text" field is truncated per turn so a runaway turn (e.g. a long
analysis) cannot dominate the digest.
"""

from __future__ import annotations

import argparse
import json
import sys
from typing import List, Optional

# Per-turn text cap. 800 chars ≈ 200 tokens, enough to convey what the
# turn was about without copying entire essays.
TEXT_CHARS_PER_TURN = 800

# Per-tool-input cap. Tool inputs (e.g. file contents in Write) can be
# huge; the tool name + a short snippet is enough for the summarizer to
# understand intent.
TOOL_INPUT_CHARS = 120


def _truncate(s: str, n: int) -> str:
    """Return ``s`` clipped to ``n`` chars with an ellipsis when clipped."""
    if len(s) <= n:
        return s
    return s[: n - 3] + "..."


def _tool_descriptor(block: dict) -> str:
    """Render a tool_use block as ``Name(short-input)`` for the digest."""
    name = block.get("name", "?")
    raw_input = block.get("input")
    if raw_input is None:
        return name
    try:
        snippet = json.dumps(raw_input, separators=(",", ":"))
    except (TypeError, ValueError):
        snippet = str(raw_input)
    return f"{name}({_truncate(snippet, TOOL_INPUT_CHARS)})"


def build_digest(path: str) -> str:
    """Build a turn-by-turn digest of the session stream-json at ``path``."""
    lines: List[str] = []
    turn = 0

    with open(path, encoding="utf-8") as handle:
        for raw in handle:
            entry = raw.strip()
            if not entry:
                continue
            try:
                event = json.loads(entry)
            except json.JSONDecodeError:
                continue
            if event.get("type") != "assistant":
                continue
            message = event.get("message") or {}
            content = message.get("content") or []
            if not content:
                continue

            turn += 1
            texts: List[str] = []
            tools: List[str] = []
            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "text":
                    texts.append(block.get("text", ""))
                elif btype == "tool_use":
                    tools.append(_tool_descriptor(block))

            lines.append(f"Turn {turn}")
            if tools:
                lines.append(f"  tools: {', '.join(tools)}")
            if texts:
                joined = "\n".join(t.strip() for t in texts if t.strip())
                if joined:
                    lines.append("  text: " + _truncate(joined, TEXT_CHARS_PER_TURN))
            lines.append("")

    if not lines:
        return "(no assistant turns recorded — session likely ended before any model output)"
    return "\n".join(lines).rstrip() + "\n"


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    parser.add_argument("input", help="Path to the stream-json session result file.")
    parser.add_argument(
        "-o", "--output", default="-",
        help="Output path; '-' (default) writes to stdout.",
    )
    args = parser.parse_args(argv)

    digest = build_digest(args.input)
    if args.output == "-":
        sys.stdout.write(digest)
    else:
        with open(args.output, "w", encoding="utf-8") as out:
            out.write(digest)
    return 0


if __name__ == "__main__":
    sys.exit(main())
