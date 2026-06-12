#!/usr/bin/env python3
"""Scan repository files for local-environment and host-dependency risks."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

TEXT_EXTENSIONS = {
    ".py",
    ".js",
    ".ts",
    ".tsx",
    ".jsx",
    ".json",
    ".yaml",
    ".yml",
    ".toml",
    ".ini",
    ".env",
    ".sh",
    ".md",
    ".txt",
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
PATTERNS = (
    ("absolute_windows_path", re.compile(r"[A-Za-z]:\\Users\\[^\\]+")),
    ("absolute_unix_path", re.compile(r"/(?:Users|home)/[^/\s]+/")),
    ("host_docker_internal", re.compile(r"\bhost\.docker\.internal\b")),
    ("localhost_reference", re.compile(r"\blocalhost\b")),
    ("loopback_reference", re.compile(r"\b127\.0\.0\.1\b")),
)
PRIVATE_IMAGE_RE = re.compile(
    r"^\s*image:\s*['\"]?(?P<image>[A-Za-z0-9.-]+/(?:[A-Za-z0-9._-]+/)?[A-Za-z0-9._-]+(?::[A-Za-z0-9._-]+)?)",
    re.IGNORECASE,
)
PRIVATE_HINTS = ("internal", "intranet", "corp", "local", "lan")
LOCALHOST_LABELS = {"localhost_reference", "loopback_reference"}
ABSOLUTE_PATH_LABELS = {"absolute_windows_path", "absolute_unix_path"}


def should_scan(path: Path, include_markdown: bool) -> bool:
    if path.suffix.lower() in TEXT_EXTENSIONS:
        if not include_markdown and path.suffix.lower() == ".md":
            return False
        return True
    return path.name in {"Dockerfile", "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}


def is_test_path(path: Path) -> bool:
    parts = {part.lower() for part in path.parts}
    name = path.name.lower()
    return bool(parts & {"test", "tests", "__tests__", "spec", "specs", "mocks", "mock", "fixtures"}) or (
        "_test." in name
        or name.startswith("test_")
        or name.endswith(".test.js")
        or name.endswith(".test.ts")
        or name.endswith(".spec.js")
        or name.endswith(".spec.ts")
    )


def is_compose_path(path: Path) -> bool:
    return path.name.lower() in {"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}


def is_comment_line(line: str) -> bool:
    stripped = line.strip()
    return stripped.startswith(("#", "//", "/*", "*"))


def is_metadata_path_field(path: Path, line: str) -> bool:
    if path.name.lower() != "metadata.json":
        return False
    return bool(re.search(r'"(?:cwd|path|project_path|workspace|artifact_root|output_dir)"\s*:', line, re.IGNORECASE))


def localhost_allowed_context(path: Path, line: str) -> bool:
    lowered = line.lower()
    if path.name.lower() == "run_tests.sh":
        return True
    if is_test_path(path):
        return True
    if is_comment_line(line):
        return True
    if is_compose_path(path):
        if any(marker in lowered for marker in ("healthcheck", "test:", "curl ", "wget ", "nc ")):
            return True
        return bool(re.search(r"['\"]?(?:localhost|127\.0\.0\.1):\d+:\d+", lowered))
    return False


def should_ignore_match(label: str, path: Path, line: str) -> bool:
    if label in LOCALHOST_LABELS:
        return localhost_allowed_context(path, line)
    if label in ABSOLUTE_PATH_LABELS:
        return is_test_path(path) or is_metadata_path_field(path, line) or is_comment_line(line)
    if label == "host_docker_internal":
        return is_comment_line(line)
    return False


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", help="Repository root to scan")
    parser.add_argument(
        "--include-markdown",
        action="store_true",
        help="Also scan markdown files for suspicious references.",
    )
    args = parser.parse_args()

    root = Path(args.root).resolve()
    findings: list[str] = []

    for path in root.rglob("*"):
        if path.is_dir():
            continue
        if any(part in IGNORED_DIRS for part in path.parts):
            continue
        if not should_scan(path, args.include_markdown):
            continue

        try:
            content = path.read_text(encoding="utf-8")
        except Exception:
            continue

        for line_number, line in enumerate(content.splitlines(), start=1):
            for label, pattern in PATTERNS:
                if pattern.search(line) and not should_ignore_match(label, path, line):
                    findings.append(f"{label}: {path}:{line_number}: {line.strip()}")
            image_match = PRIVATE_IMAGE_RE.match(line)
            if image_match:
                image_name = image_match.group("image").lower()
                if any(hint in image_name for hint in PRIVATE_HINTS):
                    findings.append(f"private_image_hint: {path}:{line_number}: {line.strip()}")

    if findings:
        print("Local dependency risks detected:")
        for finding in findings:
            print(f"- {finding}")
        return 1

    print(f"OK: no obvious local dependency risks detected under {root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
