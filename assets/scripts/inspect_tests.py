#!/usr/bin/env python3
"""Inspect whether prompt2repo test directories and the unified runner look real."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


TEST_EXTENSIONS = {".py", ".js", ".ts", ".tsx", ".jsx", ".sh", ".http", ".json", ".yaml", ".yml"}
PLACEHOLDER_RE = re.compile(r"\b(TODO|placeholder|coming soon|stub|replace with)\b", re.IGNORECASE)
IGNORED_DIRS = {".git", "node_modules", ".venv", "venv", "__pycache__", "coverage", "dist", "build"}
PROJECT_TYPES = ("pure_frontend", "pure_backend", "fullstack")
RUN_TESTS_CANDIDATES = ("run_tests.sh", "run_tests.ps1", "run_tests.py")


def collect_files(root: Path) -> list[Path]:
    files: list[Path] = []
    for path in root.rglob("*"):
        if path.is_dir():
            continue
        if any(part in IGNORED_DIRS for part in path.parts):
            continue
        if path.suffix.lower() in TEST_EXTENSIONS or path.name in RUN_TESTS_CANDIDATES:
            files.append(path)
    return files


def count_real_test_files(directory: Path) -> tuple[int, int]:
    total = 0
    non_placeholder = 0
    for path in collect_files(directory):
        if path.name in RUN_TESTS_CANDIDATES:
            continue
        total += 1
        try:
            content = path.read_text(encoding="utf-8").strip()
        except Exception:
            content = ""
        if content and not PLACEHOLDER_RE.search(content):
            non_placeholder += 1
    return total, non_placeholder


def detect_repo_root(root: Path) -> Path | None:
    repo_root = root / "repo"
    return repo_root if repo_root.is_dir() else None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", help="Package root")
    parser.add_argument("--project-type", choices=PROJECT_TYPES, help="Expected prompt2repo project type")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    repo_root = detect_repo_root(root)
    issues: list[str] = []
    summaries: list[str] = []

    if repo_root is None:
        issues.append("Missing repo/")
    else:
        for directory_name in ("unit_tests", "API_tests"):
            directory = repo_root / directory_name
            if not directory.is_dir():
                issues.append(f"Missing directory: repo/{directory_name}/")
                continue
            total, non_placeholder = count_real_test_files(directory)
            summaries.append(f"repo/{directory_name}: {total} files, {non_placeholder} non-placeholder")
            if total == 0:
                issues.append(f"No test files found under repo/{directory_name}/")
            elif non_placeholder == 0:
                issues.append(f"Only placeholder-style test files found under repo/{directory_name}/")

    run_tests = None if repo_root is None else next(
        ((repo_root / candidate) for candidate in RUN_TESTS_CANDIDATES if (repo_root / candidate).is_file()),
        None,
    )
    if run_tests is None:
        issues.append("Missing repo/run_tests.sh|run_tests.ps1|run_tests.py")
    else:
        content = run_tests.read_text(encoding="utf-8").strip()
        if not content:
            issues.append(f"{run_tests.relative_to(root).as_posix()} is empty")
        elif PLACEHOLDER_RE.search(content):
            issues.append(f"{run_tests.relative_to(root).as_posix()} still contains placeholder markers")
        else:
            summaries.append(f"{run_tests.relative_to(root).as_posix()} exists and is non-empty")

    if summaries:
        print("Test inspection summary:")
        for summary in summaries:
            print(f"- {summary}")

    if issues:
        print("Test inspection issues:")
        for issue in issues:
            print(f"- {issue}")
        return 1

    print(f"OK: test directories and unified runner look usable under {root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
