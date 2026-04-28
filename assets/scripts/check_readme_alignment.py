#!/usr/bin/env python3
"""Check README sections and basic alignment with compose services and ports."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


REQUIRED_SECTIONS = ("How to Run", "Services List", "Verification")
COMPOSE_CANDIDATES = (
    "docker-compose.yml",
    "docker-compose.yaml",
    "compose.yml",
    "compose.yaml",
)
PROJECT_TYPES = ("pure_frontend", "pure_backend", "fullstack")
PORT_RE = re.compile(r"(\d+):(\d+)")
SERVICE_RE = re.compile(r"^(\s*)([A-Za-z0-9_.-]+):\s*$")


def detect_repo_root(root: Path) -> Path | None:
    repo_root = root / "repo"
    return repo_root if repo_root.is_dir() else None


def find_compose_file(root: Path, repo_root: Path | None, explicit: str | None) -> Path | None:
    if explicit:
        path = Path(explicit)
        return path if path.is_file() else None
    if repo_root is not None:
        for candidate in COMPOSE_CANDIDATES:
            path = repo_root / candidate
            if path.is_file():
                return path
    for candidate in COMPOSE_CANDIDATES:
        path = root / candidate
        if path.is_file():
            return path
    return None


def parse_compose(compose_path: Path) -> tuple[list[str], list[str]]:
    services: list[str] = []
    ports: list[str] = []
    services_indent: int | None = None

    for raw_line in compose_path.read_text(encoding="utf-8").splitlines():
        line = raw_line.split("#", 1)[0].rstrip()
        if not line:
            continue

        indent = len(line) - len(line.lstrip(" "))
        if line.strip() == "services:":
            services_indent = indent
            continue

        if services_indent is not None:
            if indent <= services_indent:
                services_indent = None
            else:
                service_match = SERVICE_RE.match(line)
                if service_match and indent == services_indent + 2:
                    services.append(service_match.group(2))

        for port_match in PORT_RE.finditer(line):
            ports.append(port_match.group(1))

    return sorted(set(services)), sorted(set(ports))


def has_heading(readme_text: str, section: str) -> bool:
    pattern = re.compile(rf"^\s*#+\s+{re.escape(section)}\s*$", re.IGNORECASE | re.MULTILINE)
    return bool(pattern.search(readme_text))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", help="Package root")
    parser.add_argument("--readme", help="Explicit README path")
    parser.add_argument("--compose", help="Explicit compose file path")
    parser.add_argument("--project-type", choices=PROJECT_TYPES, help="Expected prompt2repo project type")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    repo_root = detect_repo_root(root)
    readme_path = Path(args.readme).resolve() if args.readme else (
        repo_root / "README.md" if repo_root is not None else root / "README.md"
    )

    if not readme_path.is_file():
        print(f"ERROR: README not found: {readme_path}", file=sys.stderr)
        return 1

    readme_text = readme_path.read_text(encoding="utf-8")
    readme_lower = readme_text.lower()
    issues: list[str] = []

    for section in REQUIRED_SECTIONS:
        if not has_heading(readme_text, section):
            issues.append(f"Missing README section: {section}")

    compose_path = find_compose_file(root, repo_root, args.compose)
    if compose_path is not None:
        services, ports = parse_compose(compose_path)
        for service in services:
            if service.lower() not in readme_lower:
                issues.append(f"README does not mention compose service: {service}")
        for port in ports:
            if not re.search(rf"\b{re.escape(port)}\b", readme_text):
                issues.append(f"README does not mention exposed port: {port}")
    else:
        print("INFO: compose file not found; skipped service/port cross-check.")

    if issues:
        print("README alignment issues:")
        for issue in issues:
            print(f"- {issue}")
        return 1

    print(f"OK: README alignment checks passed for {readme_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
