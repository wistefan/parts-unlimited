#!/usr/bin/env python3
"""Export an agent's stream-json output as a Claude Code transcript JSONL.

Purpose: Claude Spend (``npx claude-spend``) parses files at
``~/.claude/projects/<project>/<session>.jsonl`` and skips lines that do
not look like native Claude Code transcript entries. The stream-json that
``claude -p`` emits is almost the right shape, except that it lacks the
per-line ``timestamp`` field Claude Spend uses for ordering.

This script:
  * reads the concatenated stream-json produced by the bootstrap session
    loop (one file that may contain events from several sessions);
  * keeps only ``user`` and ``assistant`` events (other types — ``system``,
    ``result``, ``stream_event`` — carry no usage data that Claude Spend
    consumes);
  * adds a synthetic ISO-8601 ``timestamp`` to each kept event, spaced one
    second apart starting from ``--start-epoch`` (default: now), so the
    ordering in the UI matches the execution order; and
  * writes the result as compact JSONL to the output path, creating
    parent directories as needed.

The script is intentionally narrow — it does not re-derive tokens or
prices — so that changes to Anthropic's event shape only need to be
handled once, inside ``parse-usage.py``, and don't ripple here.
"""

from __future__ import annotations

import argparse
import datetime
import json
import os
import sys
from typing import Optional

# Claude Spend's parser reads these event types; everything else is noise
# for cost attribution.
KEEP_TYPES = frozenset({"user", "assistant"})

# One second between synthetic timestamps is the coarsest granularity
# Claude Spend's display can distinguish without extra config.
SECONDS_PER_LINE = 1


def iso_at(epoch_seconds: float) -> str:
    """Return ``epoch_seconds`` as an ISO-8601 UTC string with trailing Z."""
    dt = datetime.datetime.fromtimestamp(epoch_seconds, tz=datetime.timezone.utc)
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


def export(input_path: str, output_path: str, start_epoch: Optional[int]) -> int:
    """Transform ``input_path`` (stream-json) into ``output_path`` (transcript JSONL).

    Returns the number of events written.
    """
    start = (
        start_epoch
        if start_epoch is not None
        else int(datetime.datetime.now(datetime.timezone.utc).timestamp())
    )

    os.makedirs(os.path.dirname(output_path) or ".", exist_ok=True)

    written = 0
    with open(input_path, encoding="utf-8") as src, open(
        output_path, "w", encoding="utf-8"
    ) as dst:
        for raw in src:
            line = raw.strip()
            if not line:
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if event.get("type") not in KEEP_TYPES:
                continue
            event["timestamp"] = iso_at(start + written * SECONDS_PER_LINE)
            dst.write(json.dumps(event, separators=(",", ":")) + "\n")
            written += 1

    return written


def main(argv: Optional[list] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    parser.add_argument("input", help="Path to the stream-json result file.")
    parser.add_argument("output", help="Destination JSONL path.")
    parser.add_argument(
        "--start-epoch",
        type=int,
        default=None,
        help="Unix timestamp of the first event (default: now).",
    )
    args = parser.parse_args(argv)

    try:
        written = export(args.input, args.output, args.start_epoch)
    except FileNotFoundError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1

    print(f"Wrote {written} transcript events to {args.output}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
