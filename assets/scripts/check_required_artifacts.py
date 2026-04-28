#!/usr/bin/env python3
"""Check whether a prompt2repo package contains required acceptance artifacts."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


PROJECT_TYPES = ("pure_frontend", "pure_backend", "fullstack")
METADATA_PROJECT_TYPE_MAP = {
    "web": "pure_frontend",
    "server": "pure_backend",
    "fullstack": "fullstack",
    "android": "pure_frontend",
    "ios": "pure_frontend",
    "desktop": "pure_frontend",
}
ROOT_REQUIRED = ("metadata.json", "docs/design.md", "docs/questions.md")
ROOT_REQUIRED_DIRS = ("repo", "original_sessions")
REPO_REQUIRED = ("README.md", "unit_tests", "API_tests")
RUN_TESTS_CANDIDATES = ("run_tests.sh", "run_tests.ps1", "run_tests.py")


def load_metadata(root: Path) -> dict:
    metadata_path = root / "metadata.json"
    if not metadata_path.is_file():
        return {}
    try:
        payload = json.loads(metadata_path.read_text(encoding="utf-8"))
    except Exception:
        return {}
    return payload if isinstance(payload, dict) else {}


def normalize_value(value: object) -> str:
    return str(value or "").strip().lower()


def normalize_metadata_project_type(value: object) -> str | None:
    normalized = normalize_value(value)
    if normalized in PROJECT_TYPES:
        return normalized
    return METADATA_PROJECT_TYPE_MAP.get(normalized)


def detect_project_type(root: Path, explicit: str | None) -> str | None:
    if explicit:
        return explicit
    metadata = load_metadata(root)
    return normalize_metadata_project_type(metadata.get("project_type"))


def has_backend_content(root: Path, project_type: str | None) -> bool:
    if project_type in {"pure_backend", "fullstack"}:
        return True
    metadata = load_metadata(root)
    backend_language = normalize_value(metadata.get("backend_language"))
    backend_framework = normalize_value(metadata.get("backend_framework"))
    frontend_language = normalize_value(metadata.get("frontend_language"))
    return backend_language not in {"", "none", "null", "n/a", "na", "-", "no"} or backend_framework not in {
        "",
        "none",
        "null",
        "n/a",
        "na",
        "-",
        "no",
    } or frontend_language == "server"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", help="Package root to inspect")
    parser.add_argument("--project-type", choices=PROJECT_TYPES, help="Expected prompt2repo project type")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    if not root.exists():
        print(f"ERROR: root does not exist: {root}", file=sys.stderr)
        return 1

    project_type = detect_project_type(root, args.project_type)
    missing: list[str] = []

    for rel_path in ROOT_REQUIRED:
        if not (root / rel_path).exists():
            missing.append(rel_path)
    for rel_path in ROOT_REQUIRED_DIRS:
        if not (root / rel_path).is_dir():
            missing.append(rel_path + "/")

    repo_root = root / "repo"
    for name in REPO_REQUIRED:
        if not (repo_root / name).exists():
            missing.append(f"repo/{name}")
    if not any((repo_root / candidate).is_file() for candidate in RUN_TESTS_CANDIDATES):
        missing.append("repo/run_tests.sh|run_tests.ps1|run_tests.py")

    if has_backend_content(root, project_type) and not (root / "docs" / "api-spec.md").is_file():
        missing.append("docs/api-spec.md")

    if missing:
        print("Missing required artifacts:")
        for item in missing:
            print(f"- {item}")
        return 1

    print(f"OK: required artifacts found under {root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
