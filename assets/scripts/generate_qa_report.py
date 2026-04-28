#!/usr/bin/env python3
"""Generate a markdown QA report from a JSON description."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def load_payload(input_path: str | None, verdict: str) -> dict:
    if not input_path:
        return {
            "overall_verdict": verdict,
            "blocking_issues": [],
            "non_blocking_issues": [],
            "missing_deliverables": [],
            "runtime_evidence": [],
            "test_evidence": [],
            "suspected_fake_implementations": [],
            "repair_priority": [],
            "submit_readiness": "Undecided",
        }
    with Path(input_path).open("r", encoding="utf-8") as handle:
        return json.load(handle)


def render_issue(issue: dict) -> list[str]:
    lines = []
    issue_id = issue.get("issue_id", "unknown")
    severity = issue.get("severity", "unknown")
    rule = issue.get("rule", "unspecified")
    evidence = issue.get("evidence", "No evidence provided.")
    repair_action = issue.get("repair_action", "No repair action provided.")
    done_criteria = issue.get("done_criteria", "No done criteria provided.")
    lines.append(f"- `{issue_id}` [{severity}] {rule}")
    lines.append(f"  Evidence: {evidence}")
    lines.append(f"  Repair action: {repair_action}")
    lines.append(f"  Done criteria: {done_criteria}")
    return lines


def render_list(title: str, items: list) -> list[str]:
    lines = [f"## {title}", ""]
    if not items:
        lines.append("- None")
        lines.append("")
        return lines
    for item in items:
        if isinstance(item, dict):
            lines.extend(render_issue(item))
        else:
            lines.append(f"- {item}")
    lines.append("")
    return lines


def build_markdown(payload: dict) -> str:
    lines = [
        "# QA Report",
        "",
        f"## Overall Verdict",
        "",
        f"- {payload.get('overall_verdict', 'UNKNOWN')}",
        "",
    ]
    lines.extend(render_list("Blocking Issues", payload.get("blocking_issues", [])))
    lines.extend(render_list("Non-Blocking Issues", payload.get("non_blocking_issues", [])))
    lines.extend(render_list("Missing Deliverables", payload.get("missing_deliverables", [])))
    lines.extend(render_list("Runtime Evidence", payload.get("runtime_evidence", [])))
    lines.extend(render_list("Test Evidence", payload.get("test_evidence", [])))
    lines.extend(
        render_list(
            "Suspected Fake Implementations",
            payload.get("suspected_fake_implementations", []),
        )
    )
    lines.extend(render_list("Repair Priority", payload.get("repair_priority", [])))
    lines.extend(
        [
            "## Submit Readiness",
            "",
            f"- {payload.get('submit_readiness', 'Undecided')}",
            "",
        ]
    )
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", help="Path to a JSON payload file")
    parser.add_argument(
        "--verdict",
        default="REWORK",
        choices=("PASS", "REWORK", "FAIL"),
        help="Fallback verdict when no JSON input is provided",
    )
    parser.add_argument("--output", help="Optional markdown output path")
    args = parser.parse_args()

    try:
        payload = load_payload(args.input, args.verdict)
        markdown = build_markdown(payload)
    except Exception as exc:  # pragma: no cover - CLI guard
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1

    if args.output:
        Path(args.output).write_text(markdown, encoding="utf-8")
    else:
        print(markdown)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
