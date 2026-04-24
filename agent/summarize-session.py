#!/usr/bin/env python3
"""Build a compact textual digest or ready-made Context Summary block.

Two output modes — both read the same Claude Code stream-json session:

* ``--format digest`` (default, legacy): a turn-by-turn trace suitable
  as the user message for a separate ``claude -p`` summarizer call.
* ``--format summary-block``: a fully-formed ``### Context Summary``
  markdown block, produced DETERMINISTICALLY from the tool trace — no
  Claude call, no extra tokens. Replaces the old "digest → claude -p"
  pipeline for the chained-session handoff path.

Why skip the Claude-call summarizer:

* It cost ~30–50K tokens per invocation and fired on every max-turns
  hit (2–3× per step in practice).
* Its output is a paraphrase of what already happened — but for the
  chained-session handoff, the next session really only needs two
  concrete signals: (a) which files the prior session already read so
  it does not re-Read them, and (b) what the prior session was about
  to do next. Both are mechanically extractable from the tool trace
  without any LLM involvement.

The legacy ``digest`` format is kept for diagnostic / offline use; the
``summary-block`` format is what the bootstrap consumes.
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


# --- summary-block output -------------------------------------------------
#
# Cap on the last-narrative excerpt. 600 chars ≈ 150 tokens — enough to
# carry intent ("Now let me wire up the tracing config...") without
# dragging essay-length reflections forward.
LAST_TEXT_CHARS = 600

# Cap on the file list length. At 50+ reads the list is itself noise;
# capping keeps the Context Summary under ~400 tokens even for a full
# 20-turn read-heavy session.
MAX_FILES_LISTED = 25


def _iter_assistant(path: str):
    """Yield parsed assistant events from the stream-json session."""
    with open(path, encoding="utf-8") as handle:
        for raw in handle:
            entry = raw.strip()
            if not entry:
                continue
            try:
                event = json.loads(entry)
            except json.JSONDecodeError:
                continue
            if event.get("type") == "assistant":
                yield event


def _collect(path: str) -> dict:
    """Walk the session and bucket tool calls by kind.

    Uses dict keys (not sets) to preserve first-seen order — the agent
    usually reads files in a semantically meaningful order and the
    order is useful context for a human reviewer.
    """
    read_paths: dict[str, None] = {}
    edit_paths: dict[str, None] = {}
    write_paths: dict[str, None] = {}
    bash_cmds: List[str] = []
    last_text: Optional[str] = None
    turn_count = 0

    for event in _iter_assistant(path):
        content = (event.get("message") or {}).get("content") or []
        has_tool_or_text = False
        for block in content:
            if not isinstance(block, dict):
                continue
            btype = block.get("type")
            if btype == "text":
                text = (block.get("text") or "").strip()
                if text:
                    last_text = text
                    has_tool_or_text = True
            elif btype == "tool_use":
                has_tool_or_text = True
                name = block.get("name", "")
                raw_input = block.get("input") or {}
                if not isinstance(raw_input, dict):
                    continue
                if name == "Read":
                    fp = raw_input.get("file_path")
                    if fp:
                        read_paths.setdefault(fp, None)
                elif name == "Edit":
                    fp = raw_input.get("file_path")
                    if fp:
                        edit_paths.setdefault(fp, None)
                elif name == "Write":
                    fp = raw_input.get("file_path")
                    if fp:
                        write_paths.setdefault(fp, None)
                elif name == "Bash":
                    cmd = raw_input.get("command")
                    if isinstance(cmd, str) and cmd.strip():
                        bash_cmds.append(cmd.strip())
        if has_tool_or_text:
            turn_count += 1

    return {
        "read": list(read_paths.keys()),
        "edit": list(edit_paths.keys()),
        "write": list(write_paths.keys()),
        "bash": bash_cmds,
        "last_text": last_text,
        "turns": turn_count,
    }


def _format_file_list(paths: List[str]) -> str:
    """Render a file list, capped so the summary stays small."""
    if not paths:
        return "(none)"
    if len(paths) <= MAX_FILES_LISTED:
        return ", ".join(f"`{p}`" for p in paths)
    shown = ", ".join(f"`{p}`" for p in paths[:MAX_FILES_LISTED])
    return f"{shown}, … (+{len(paths) - MAX_FILES_LISTED} more)"


# Commands matched via regex against the command's first token. Using
# the first token catches `helm-docs`, `helm`, `go`, `mvn`, `./build.sh`,
# `kubectl`, etc. — including hyphenated CLIs that a plain substring
# match would miss. Missing these led to real stuck-loop behaviour: a
# step that produced its artefacts via `helm-docs` looked "empty" to
# the summarizer and the next session re-ran the generator.
import re

_NOTABLE_CMD_RE = re.compile(
    r"""^(?:
        # Build/test runners
        go|npm|npx|yarn|pnpm|pip|pytest|tox|mvn|gradle|cargo|make|
        # Any shell script from the repo root (typical project build scripts)
        \./[\w.-]+\.sh|
        # Kubernetes / Helm ecosystem
        kubectl|helm(?:-[a-z]+)?|kubeconform|kustomize|
        # Containers
        docker|podman|buildah|
        # Git *mutations only* (status / log / diff are orientation noise)
        git\s+(?:commit|push|merge|rebase|tag|checkout|reset|stash|cherry-pick|revert|add)|
        # File-system side effects that can silently produce artefacts
        tar|curl|wget|cp|mv|mkdir|touch|chmod|chown
    )\b""",
    re.VERBOSE,
)


def _select_notable_bash(commands: List[str]) -> List[str]:
    """Pick commands a human reviewer would want to see at a glance.

    Excludes trivial orientation probes (``git status``, ``ls``, ``cd``,
    ``cat``). Includes build/test commands, git mutations, and anything
    else that can produce persistent state — hyphenated CLIs and project
    scripts included.
    """
    notable: List[str] = []
    for cmd in commands:
        head = cmd.splitlines()[0].strip()
        if _NOTABLE_CMD_RE.match(head):
            notable.append(head[:200])
    # De-dupe while preserving order.
    seen: dict[str, None] = {}
    for cmd in notable:
        seen.setdefault(cmd, None)
    return list(seen.keys())[:15]


def _tail_tool_calls(path: str, n: int = 5) -> List[str]:
    """Return the last ``n`` tool calls across all turns, formatted.

    Primary purpose: carry forward the *intent* of the prior session
    when it ran out of turns mid-task. The filtered ``notable`` list
    drops orientation probes, but the last-N list includes EVERYTHING
    — because when the agent hit the wall on e.g. a ``Read`` of a file
    it was about to Edit, that Read IS the breadcrumb the next session
    needs. Broad inclusion beats narrow filtering for this tail slice.
    """
    events: List[str] = []
    for event in _iter_assistant(path):
        content = (event.get("message") or {}).get("content") or []
        for block in content:
            if not isinstance(block, dict):
                continue
            if block.get("type") != "tool_use":
                continue
            name = block.get("name", "?")
            raw_input = block.get("input")
            if isinstance(raw_input, dict):
                detail = (
                    raw_input.get("command")
                    or raw_input.get("file_path")
                    or raw_input.get("pattern")
                    or ""
                )
                if isinstance(detail, str):
                    detail = detail.splitlines()[0][:160] if detail else ""
                else:
                    detail = str(detail)[:160]
            else:
                detail = ""
            events.append(f"{name}({detail})" if detail else name)
    return events[-n:]


def build_summary_block(path: str) -> str:
    """Produce a deterministic ``### Context Summary`` block."""
    state = _collect(path)

    if state["turns"] == 0:
        return (
            "### Context Summary\n\n"
            "- **Done this session:** (no assistant turns recorded — "
            "the session likely errored before any model output)\n"
            "- **State:** unknown; see bootstrap logs.\n"
            "- **Next:** re-run this session from the original task prompt.\n"
            "- **Pitfalls:** the previous invocation produced no output.\n"
        )

    parts = [
        "### Context Summary",
        "",
        f"_Auto-generated from the tool trace of the previous session "
        f"({state['turns']} turns). Treat as authoritative — it lists "
        f"exactly what the prior session did, not a paraphrase._",
        "",
        "- **Files already READ (do NOT re-read unless you need to modify them):**",
        f"  {_format_file_list(state['read'])}",
        "- **Files EDITED:**",
        f"  {_format_file_list(state['edit'])}",
        "- **Files WRITTEN (created/overwritten):**",
        f"  {_format_file_list(state['write'])}",
    ]

    notable = _select_notable_bash(state["bash"])
    if notable:
        parts.append(
            "- **Notable bash commands run (any of these may have "
            "produced files visible only via `git status`, NOT via the "
            "Write/Edit tool — e.g. `helm-docs` generates README.md, "
            "`curl | tar` drops binaries under `bin/`, test runners "
            "create coverage files):**"
        )
        for cmd in notable:
            parts.append(f"  - `{cmd}`")

    # The last few tool calls, unfiltered — load-bearing when the prior
    # session produced no text narrative (all-tool-use turns). Without
    # this the next session has no breadcrumb for "what was I doing".
    tail = _tail_tool_calls(path, n=5)
    if tail:
        parts.append("- **Last 5 tool calls (most recent last):**")
        for call in tail:
            parts.append(f"  - `{call}`")

    if state["last_text"]:
        snippet = state["last_text"].strip()
        if len(snippet) > LAST_TEXT_CHARS:
            snippet = snippet[: LAST_TEXT_CHARS - 3] + "..."
        parts.append("- **Last narrative (what the session was about to do next):**")
        # Indent so multi-line text renders as one bullet in Markdown.
        for line in snippet.splitlines():
            parts.append(f"  > {line}")
    else:
        parts.append(
            "- **Last narrative:** (none — the prior session ended "
            "mid-tool-call with no assistant text output; rely on the "
            "`Last 5 tool calls` list above to infer intent)"
        )

    parts.append(
        "- **State:** working-tree state is captured separately in the "
        '`Verified Workspace State` section of this prompt. **CRITICAL:** '
        "if that section shows untracked files (e.g. `?? some/file.md`) "
        "that are NOT in the `Files WRITTEN` list above, they were "
        "produced by a bash side-effect (often `helm-docs`, `curl`, a "
        "generator script, etc.). They ARE part of your work-in-progress "
        "— do NOT regenerate them; inspect, refine if needed, and commit."
    )
    parts.append(
        "- **Next:** continue from the last narrative / last tool calls "
        "above. The prior session likely ran out of turns mid-task — "
        "pick up the next concrete action. Before re-running any "
        "generator (`helm-docs`, `helm template`, `build.sh`, linters), "
        "first check whether its expected output already exists in the "
        "working tree (per `Verified Workspace State`). If it does, "
        "skip the regeneration and move to the next unfinished item."
    )
    parts.append(
        "- **Pitfalls:** none programmatically detected; see the bash "
        "commands list above for anything that may have failed."
    )

    return "\n".join(parts) + "\n"


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    parser.add_argument("input", help="Path to the stream-json session result file.")
    parser.add_argument(
        "-o", "--output", default="-",
        help="Output path; '-' (default) writes to stdout.",
    )
    parser.add_argument(
        "--format", choices=("digest", "summary-block"), default="digest",
        help="Output shape: 'digest' (turn-by-turn trace) or "
             "'summary-block' (a ready-to-use ### Context Summary block).",
    )
    args = parser.parse_args(argv)

    if args.format == "summary-block":
        out_text = build_summary_block(args.input)
    else:
        out_text = build_digest(args.input)

    if args.output == "-":
        sys.stdout.write(out_text)
    else:
        with open(args.output, "w", encoding="utf-8") as out:
            out.write(out_text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
