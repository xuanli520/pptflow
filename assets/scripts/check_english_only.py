#!/usr/bin/env python3
"""Scan a repository tree for Chinese characters in likely text files."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

CJK_RE = re.compile(r"[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]")
TEXT_EXTENSIONS = {
    ".md",
    ".txt",
    ".json",
    ".jsonl",
    ".log",
    ".yaml",
    ".yml",
    ".toml",
    ".ini",
    ".env",
    ".py",
    ".js",
    ".ts",
    ".tsx",
    ".jsx",
    ".html",
    ".css",
    ".scss",
    ".sh",
    ".sql",
}
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


def should_scan(path: Path) -> bool:
    return path.suffix.lower() in TEXT_EXTENSIONS or path.name in {"Dockerfile", "README"}


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
            content = path.read_text(encoding="utf-8")
        except Exception:
            continue
        for line_number, line in enumerate(content.splitlines(), start=1):
            if CJK_RE.search(line):
                findings.append(f"{path}:{line_number}: {line.strip()}")

    if findings:
        print("Chinese text detected:")
        for finding in findings:
            print(finding)
        return 1

    print(f"OK: no Chinese text detected under {root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
