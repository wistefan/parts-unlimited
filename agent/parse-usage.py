#!/usr/bin/env python3
"""Parse Claude Code stream-json output and emit token usage metrics.

Produces a compact, human-readable markdown table meant for Taiga ticket
comments. Preserves the shape of the legacy inline summary so existing
downstream consumers keep working.

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


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument(
        "result_file", help="Path to the Claude Code stream-json output file."
    )
    parser.add_argument(
        "--format",
        choices=["markdown"],
        default="markdown",
        help="Output format (default: markdown).",
    )
    args = parser.parse_args(argv)

    sessions = parse_stream_json(args.result_file)
    print(format_markdown(sessions))
    return 0


if __name__ == "__main__":
    sys.exit(main())
