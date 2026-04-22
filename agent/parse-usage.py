#!/usr/bin/env python3
"""Parse Claude Code stream-json output and emit token usage metrics.

Two output formats are supported:

* ``markdown`` — a compact, human-readable table meant for Taiga ticket
  comments. Preserves the shape of the legacy inline summary so existing
  downstream consumers keep working.
* ``prometheus`` — exposition-format text for the Prometheus Pushgateway.
  Emits job-level gauges, plus per-session, per-turn, and per-tool-call
  gauges labeled by session, turn, and tool name. Common identity labels
  (``ticket_id``, ``agent_id``, ``mode``, ...) are applied by the
  Pushgateway via the URL grouping path and are not embedded here.

The input file is the ``--output-format stream-json`` output of one or
more ``claude`` invocations concatenated together. Sessions are delimited
by ``system`` events with subtype ``init``; if the file does not begin
with one, the first ``assistant`` message starts session 1.
"""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass, field
from typing import List, Optional, Tuple

# --- Pricing constants (USD per million tokens) ---
# Used only for the markdown cost estimate shown in Taiga comments.
# Figures reflect Anthropic's public Opus 4.x pricing as of 2026-01.
PRICE_INPUT_PER_MTOK = 15.0
PRICE_OUTPUT_PER_MTOK = 75.0
PRICE_CACHE_WRITE_PER_MTOK = 18.75
PRICE_CACHE_READ_PER_MTOK = 1.50
TOKENS_PER_MILLION = 1_000_000

# Stream-json event discriminators.
EVENT_TYPE_SYSTEM = "system"
EVENT_TYPE_ASSISTANT = "assistant"
SYSTEM_SUBTYPE_INIT = "init"
CONTENT_BLOCK_TOOL_USE = "tool_use"


@dataclass
class TurnUsage:
    """Token usage and tool calls for a single assistant turn within a session."""

    session: int
    turn: int
    input_tokens: int = 0
    output_tokens: int = 0
    cache_write: int = 0
    cache_read: int = 0
    tools: List[str] = field(default_factory=list)

    @property
    def cache_hit_ratio(self) -> float:
        """Fraction of input served from cache: ``cache_read / (input + cache_read)``.

        Returns 0.0 when there is no input at all (first-turn edge case).
        """
        denom = self.input_tokens + self.cache_read
        return (self.cache_read / denom) if denom > 0 else 0.0


@dataclass
class Session:
    """Aggregated usage for one Claude CLI invocation (one session)."""

    number: int
    turns: List[TurnUsage] = field(default_factory=list)
    prompt_bytes: int = 0

    @property
    def input_tokens(self) -> int:
        return sum(t.input_tokens for t in self.turns)

    @property
    def output_tokens(self) -> int:
        return sum(t.output_tokens for t in self.turns)

    @property
    def cache_write(self) -> int:
        return sum(t.cache_write for t in self.turns)

    @property
    def cache_read(self) -> int:
        return sum(t.cache_read for t in self.turns)

    @property
    def cache_hit_ratio(self) -> float:
        denom = self.input_tokens + self.cache_read
        return (self.cache_read / denom) if denom > 0 else 0.0


def parse_stream_json(path: str) -> List[Session]:
    """Parse ``path`` as stream-json and return one ``Session`` per init event.

    Malformed JSON lines and lines without usage data are skipped silently —
    the stream-json stream mixes many message types and a robust parser
    should not fail on unexpected shapes.
    """
    sessions: List[Session] = []
    current: Optional[Session] = None
    turn_counter = 0

    with open(path, encoding="utf-8") as handle:
        for raw in handle:
            line = raw.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue

            mtype = msg.get("type")

            if mtype == EVENT_TYPE_SYSTEM and msg.get("subtype") == SYSTEM_SUBTYPE_INIT:
                current = Session(number=len(sessions) + 1)
                sessions.append(current)
                turn_counter = 0
                continue

            if mtype != EVENT_TYPE_ASSISTANT:
                continue

            if current is None:
                current = Session(number=1)
                sessions.append(current)
                turn_counter = 0

            message = msg.get("message") or {}
            usage = message.get("usage") or {}
            if not usage:
                continue

            turn_counter += 1
            turn = TurnUsage(
                session=current.number,
                turn=turn_counter,
                input_tokens=int(usage.get("input_tokens", 0) or 0),
                output_tokens=int(usage.get("output_tokens", 0) or 0),
                cache_write=int(usage.get("cache_creation_input_tokens", 0) or 0),
                cache_read=int(usage.get("cache_read_input_tokens", 0) or 0),
            )

            for block in message.get("content") or []:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == CONTENT_BLOCK_TOOL_USE and block.get("name"):
                    turn.tools.append(str(block["name"]))

            current.turns.append(turn)

    return sessions


def _job_totals(sessions: List[Session]) -> Tuple[int, int, int, int, int]:
    """Return ``(input, output, cache_write, cache_read, turns)`` across all sessions."""
    total_input = sum(s.input_tokens for s in sessions)
    total_output = sum(s.output_tokens for s in sessions)
    total_cache_write = sum(s.cache_write for s in sessions)
    total_cache_read = sum(s.cache_read for s in sessions)
    total_turns = sum(len(s.turns) for s in sessions)
    return total_input, total_output, total_cache_write, total_cache_read, total_turns


def format_markdown(sessions: List[Session]) -> str:
    """Render a markdown table summarising job-level token usage and cost."""
    total_input, total_output, total_cache_write, total_cache_read, total_turns = _job_totals(
        sessions
    )

    i_cost = total_input * PRICE_INPUT_PER_MTOK / TOKENS_PER_MILLION
    o_cost = total_output * PRICE_OUTPUT_PER_MTOK / TOKENS_PER_MILLION
    cw_cost = total_cache_write * PRICE_CACHE_WRITE_PER_MTOK / TOKENS_PER_MILLION
    cr_cost = total_cache_read * PRICE_CACHE_READ_PER_MTOK / TOKENS_PER_MILLION
    total_cost = i_cost + o_cost + cw_cost + cr_cost

    rows = [
        "| Metric | Tokens | Est. Cost |",
        "|---|---:|---:|",
        f"| Input | {total_input:,} | ${i_cost:.2f} |",
        f"| Output | {total_output:,} | ${o_cost:.2f} |",
        f"| Cache write | {total_cache_write:,} | ${cw_cost:.2f} |",
        f"| Cache read | {total_cache_read:,} | ${cr_cost:.2f} |",
        f"| **Total** | | **${total_cost:.2f}** |",
        f"| Turns | {total_turns} | |",
        f"| Sessions | {len(sessions)} | |",
    ]
    return "\n".join(rows)


def _escape_label_value(value: str) -> str:
    """Escape a Prometheus label value per the exposition format spec."""
    return value.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


def format_prometheus(sessions: List[Session], duration_seconds: float) -> str:
    """Render Prometheus exposition text for job/session/turn/tool metrics."""
    lines: List[str] = []
    total_input, total_output, total_cache_write, total_cache_read, total_turns = _job_totals(
        sessions
    )

    job_metrics = [
        ("agent_tokens_input_total", total_input),
        ("agent_tokens_output_total", total_output),
        ("agent_tokens_cache_write_total", total_cache_write),
        ("agent_tokens_cache_read_total", total_cache_read),
        ("agent_turns_total", total_turns),
        ("agent_sessions_total", len(sessions)),
        ("agent_duration_seconds", duration_seconds),
    ]
    for name, value in job_metrics:
        lines.append(f"# TYPE {name} gauge")
        lines.append(f"{name} {value}")

    session_declared: List[str] = []

    def declare_session(name: str) -> None:
        if name not in session_declared:
            lines.append(f"# TYPE {name} gauge")
            session_declared.append(name)

    for s in sessions:
        label = f'session="{s.number}"'
        for name, value in [
            ("agent_session_tokens_input", s.input_tokens),
            ("agent_session_tokens_output", s.output_tokens),
            ("agent_session_tokens_cache_write", s.cache_write),
            ("agent_session_tokens_cache_read", s.cache_read),
            ("agent_session_turns", len(s.turns)),
            ("agent_session_cache_hit_ratio", round(s.cache_hit_ratio, 6)),
            ("agent_session_prompt_bytes", s.prompt_bytes),
        ]:
            declare_session(name)
            lines.append(f"{name}{{{label}}} {value}")

    turn_declared: List[str] = []

    def declare_turn(name: str) -> None:
        if name not in turn_declared:
            lines.append(f"# TYPE {name} gauge")
            turn_declared.append(name)

    for s in sessions:
        for t in s.turns:
            label = f'session="{t.session}",turn="{t.turn}"'
            for name, value in [
                ("agent_turn_tokens_input", t.input_tokens),
                ("agent_turn_tokens_output", t.output_tokens),
                ("agent_turn_tokens_cache_write", t.cache_write),
                ("agent_turn_tokens_cache_read", t.cache_read),
                ("agent_turn_cache_hit_ratio", round(t.cache_hit_ratio, 6)),
            ]:
                declare_turn(name)
                lines.append(f"{name}{{{label}}} {value}")

    # Tool-use: aggregate counts per (session, turn, tool) to avoid duplicate
    # label sets when the same tool is called multiple times in one turn.
    # ``agent_tool_tokens_after`` is a proxy for tool-result size — the
    # cumulative input+cache_read tokens observed on the turn immediately
    # following the tool call. Multiple tools in the same turn share this
    # value, which is an accepted attribution limitation.
    tool_call_counts: dict = {}
    tool_tokens_after: dict = {}
    for s in sessions:
        for idx, t in enumerate(s.turns):
            next_tokens = 0
            if idx + 1 < len(s.turns):
                nxt = s.turns[idx + 1]
                next_tokens = nxt.input_tokens + nxt.cache_read
            for tool in t.tools:
                key = (t.session, t.turn, tool)
                tool_call_counts[key] = tool_call_counts.get(key, 0) + 1
                tool_tokens_after[key] = next_tokens

    if tool_call_counts:
        lines.append("# TYPE agent_tool_calls_total gauge")
        lines.append("# TYPE agent_tool_tokens_after gauge")
        for (session, turn, tool), count in tool_call_counts.items():
            labels = (
                f'session="{session}",turn="{turn}",tool="{_escape_label_value(tool)}"'
            )
            lines.append(f"agent_tool_calls_total{{{labels}}} {count}")
            lines.append(f"agent_tool_tokens_after{{{labels}}} {tool_tokens_after[(session, turn, tool)]}")

    lines.append("")
    return "\n".join(lines)


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument(
        "result_file", help="Path to the Claude Code stream-json output file."
    )
    parser.add_argument(
        "--format",
        choices=["markdown", "prometheus"],
        default="markdown",
        help="Output format (default: markdown).",
    )
    parser.add_argument(
        "--duration",
        type=float,
        default=0.0,
        help="Wall-clock job duration in seconds (prometheus format only).",
    )
    args = parser.parse_args(argv)

    sessions = parse_stream_json(args.result_file)

    if args.format == "markdown":
        print(format_markdown(sessions))
    else:
        sys.stdout.write(format_prometheus(sessions, args.duration))
    return 0


if __name__ == "__main__":
    sys.exit(main())
