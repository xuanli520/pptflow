#!/usr/bin/env python3
"""Heuristically scan code for suspicious fake-implementation markers."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

CODE_EXTENSIONS = {".py", ".js", ".ts", ".tsx", ".jsx", ".java", ".go", ".rb", ".php", ".cs"}
IGNORED_DIRS = {
    ".git",
    "node_modules",
    ".venv",
    "venv",
    "dist",
    "build",
    ".next",
    "__pycache__",
    "coverage",
}
PLACEHOLDER_RE = re.compile(r"\b(mock data|fake data|placeholder data|stub response)\b", re.IGNORECASE)
TODO_RE = re.compile(r"\b(TODO|FIXME)\b.*\b(implement|placeholder|stub)\b", re.IGNORECASE)
STATIC_SUCCESS_RE = re.compile(r"\breturn\b[^\n]{0,60}(?:['\"](?:success|ok|done)['\"])", re.IGNORECASE)
STATIC_AUTH_RE = re.compile(r"\b(?:authenticated|authorized|loggedIn|success)\b\s*:\s*(?:true|True)", re.IGNORECASE)
STATIC_COLLECTION_RE = re.compile(r"\b(?:return|res\.json|jsonify)\s*\(\s*\[\s*\{?", re.IGNORECASE)
AUTH_HINT_RE = re.compile(r"\b(login|signin|sign[-_]?in|auth|authenticate)\w*\b", re.IGNORECASE)
QUERY_HINT_RE = re.compile(r"\b(list|query|search|getall|get_list|fetch)\w*\b", re.IGNORECASE)


def should_scan(path: Path) -> bool:
    return path.suffix.lower() in CODE_EXTENSIONS


def window_text(lines: list[str], index: int) -> str:
    start = max(0, index - 2)
    end = min(len(lines), index + 3)
    return "\n".join(lines[start:end])


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", help="Repository root to scan")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    findings: list[str] = []

    for path in root.rglob("*"):
        if path.is_dir():
            continue
        if any(part in IGNORED_DIRS for part in path.parts):
            continue
        if not should_scan(path):
            continue

        try:
            lines = path.read_text(encoding="utf-8").splitlines()
        except Exception:
            continue

        for index, line in enumerate(lines):
            context = window_text(lines, index)
            stripped = line.strip()

            if PLACEHOLDER_RE.search(line) or TODO_RE.search(line):
                findings.append(f"placeholder_marker: {path}:{index + 1}: {stripped}")
                continue

            if STATIC_SUCCESS_RE.search(line) and AUTH_HINT_RE.search(context):
                findings.append(f"static_success_return: {path}:{index + 1}: {stripped}")
                continue

            if STATIC_AUTH_RE.search(line) and AUTH_HINT_RE.search(context):
                findings.append(f"static_auth_flag: {path}:{index + 1}: {stripped}")
                continue

            if STATIC_COLLECTION_RE.search(line) and QUERY_HINT_RE.search(context):
                findings.append(f"static_collection_response: {path}:{index + 1}: {stripped}")

    if findings:
        print("Suspicious fake-implementation markers detected:")
        for finding in findings:
            print(f"- {finding}")
        return 1

    print(f"OK: no obvious fake-implementation markers detected under {root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
