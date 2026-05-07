#!/usr/bin/env python3
"""Run a prompt2repo acceptance preflight and emit a structured QA payload."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path


COMPOSE_CANDIDATES = (
    "docker-compose.yml",
    "docker-compose.yaml",
    "compose.yml",
    "compose.yaml",
)
PROJECT_TYPES = ("pure_frontend", "pure_backend", "fullstack")
METADATA_PROJECT_TYPE_MAP = {
    "web": "pure_frontend",
    "server": "pure_backend",
    "fullstack": "fullstack",
    "android": "pure_frontend",
    "ios": "pure_frontend",
    "desktop": "pure_frontend",
}
PROMPT_ENGLISH_RATIO_THRESHOLD = 0.70
CJK_RE = re.compile(r"[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]")
LATIN_RE = re.compile(r"[A-Za-z]")


def run_script(script: Path, root: Path, extra_args: list[str] | None = None) -> subprocess.CompletedProcess[str]:
    command = [sys.executable, str(script), str(root)]
    if extra_args:
        command.extend(extra_args)
    return subprocess.run(command, capture_output=True, text=True, cwd=root)


def make_issue(
    issue_id: str,
    severity: str,
    rule: str,
    evidence: str,
    repair_action: str,
    done_criteria: str,
) -> dict:
    return {
        "issue_id": issue_id,
        "severity": severity,
        "rule": rule,
        "evidence": evidence.strip() or "No evidence captured.",
        "repair_action": repair_action,
        "done_criteria": done_criteria,
    }


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


def calc_prompt_english_ratio(text: str) -> float:
    latin_count = len(LATIN_RE.findall(text))
    cjk_count = len(CJK_RE.findall(text))
    total_letters = latin_count + cjk_count
    if total_letters == 0:
        return 0.0
    return latin_count / total_letters


def resolve_prompt_language(root: Path, explicit: str) -> str:
    if explicit != "auto":
        return explicit

    metadata = load_metadata(root)
    prompt = str(metadata.get("prompt") or "").strip()
    if not prompt:
        return "non-english"

    latin_count = len(LATIN_RE.findall(prompt))
    cjk_count = len(CJK_RE.findall(prompt))

    if latin_count == 0 and cjk_count > 0:
        return "non-english"
    if cjk_count == 0 and latin_count > 0:
        return "english"
    if calc_prompt_english_ratio(prompt) > PROMPT_ENGLISH_RATIO_THRESHOLD:
        return "english"
    return "non-english"


def detect_project_type(root: Path, explicit: str | None) -> str | None:
    if explicit:
        return explicit
    metadata = load_metadata(root)
    return normalize_metadata_project_type(metadata.get("project_type"))


def detect_repo_root(root: Path) -> Path | None:
    repo_root = root / "repo"
    return repo_root if repo_root.is_dir() else None


def find_compose(root: Path, repo_root: Path | None) -> Path | None:
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


def maybe_render_markdown(payload: dict, output_json: Path | None, output_md: Path | None, report_script: Path) -> None:
    if output_md is None:
        return

    if output_json is not None:
        input_path = output_json
    else:
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", suffix=".json", delete=False) as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2)
            input_path = Path(handle.name)

    subprocess.run(
        [
            sys.executable,
            str(report_script),
            "--input",
            str(input_path),
            "--output",
            str(output_md),
        ],
        check=True,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", help="Package root to inspect")
    parser.add_argument("--project-type", choices=PROJECT_TYPES, help="Expected prompt2repo project type")
    parser.add_argument(
        "--prompt-language",
        choices=("auto", "english", "non-english"),
        default="auto",
        help="Prompt language. Auto detects from metadata.json.prompt; use english to force the Chinese scan.",
    )
    parser.add_argument(
        "--runtime-command",
        help="Optional runtime verification command, for example: docker compose up --build -d",
    )
    parser.add_argument(
        "--runtime-timeout-seconds",
        type=int,
        default=600,
        help="Timeout for the runtime verification command.",
    )
    parser.add_argument("--output-json", help="Optional JSON payload output path.")
    parser.add_argument("--output-md", help="Optional markdown report output path.")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    project_type = detect_project_type(root, args.project_type)
    prompt_language = resolve_prompt_language(root, args.prompt_language)
    repo_root = detect_repo_root(root)
    scripts_dir = Path(__file__).resolve().parent
    payload = {
        "overall_verdict": "PASS",
        "blocking_issues": [],
        "non_blocking_issues": [],
        "missing_deliverables": [],
        "runtime_evidence": [],
        "test_evidence": [],
        "suspected_fake_implementations": [],
        "repair_priority": [],
        "submit_readiness": "Ready to submit",
    }

    if project_type and project_type != "pure_frontend" and find_compose(root, repo_root) is None:
        payload["blocking_issues"].append(
            make_issue(
                "compose-missing",
                "blocking",
                "3.1.1 / 3.7.1",
                "No compose file was found in repo/ for a non-pure-frontend project.",
                "Add a compose entry point inside repo/ and declare all runtime dependencies explicitly.",
                "A compose file exists in repo/ and docker-based startup instructions can be validated.",
            )
        )

    artifact_result = run_script(
        scripts_dir / "check_required_artifacts.py",
        root,
        ["--project-type", project_type] if project_type else None,
    )
    if artifact_result.returncode != 0:
        payload["blocking_issues"].append(
            make_issue(
                "required-artifacts-missing",
                "blocking",
                "3.2.1",
                artifact_result.stdout or artifact_result.stderr,
                "Add the missing package artifacts and keep docs/, repo/, metadata.json, and one original session marker in the expected locations.",
                "Artifact checks pass and the package tree includes all required files and directories.",
            )
        )
        payload["missing_deliverables"].extend(
            line[2:] for line in artifact_result.stdout.splitlines() if line.startswith("- ")
        )

    readme_result = run_script(
        scripts_dir / "check_readme_alignment.py",
        root,
        ["--project-type", project_type] if project_type else None,
    )
    if readme_result.returncode != 0:
        payload["blocking_issues"].append(
            make_issue(
                "readme-misalignment",
                "blocking",
                "3.1.1 / 3.7.4",
                readme_result.stdout or readme_result.stderr,
                "Update repo/README.md sections, service names, ports, and verification paths so they match the repository.",
                "README alignment checks pass and the documented runbook matches the actual package layout.",
            )
        )

    local_dep_result = run_script(scripts_dir / "check_local_dependency.py", root)
    if local_dep_result.returncode != 0:
        payload["blocking_issues"].append(
            make_issue(
                "local-dependency-risk",
                "blocking",
                "3.1.1 / 3.7.3",
                local_dep_result.stdout or local_dep_result.stderr,
                "Remove absolute paths, host-only service references, and suspicious private-image dependencies.",
                "The local dependency scan passes without suspicious host-environment references.",
            )
        )

    if prompt_language == "english":
        english_result = run_script(scripts_dir / "check_english_only.py", root)
        if english_result.returncode != 0:
            payload["blocking_issues"].append(
                make_issue(
                    "english-only-violation",
                    "blocking",
                    "General / English red line",
                    english_result.stdout or english_result.stderr,
                    "Remove Chinese text from deliverables that must remain English-only.",
                    "The English-only scan passes for the package tree.",
                )
            )

    test_result = run_script(
        scripts_dir / "inspect_tests.py",
        root,
        ["--project-type", project_type] if project_type else None,
    )
    payload["test_evidence"].append((test_result.stdout or test_result.stderr).strip() or "No test evidence captured.")
    if test_result.returncode != 0:
        payload["non_blocking_issues"].append(
            make_issue(
                "test-structure-gap",
                "major",
                "3.3.4",
                test_result.stdout or test_result.stderr,
                "Add real unit and API tests under repo/ and make sure repo/run_tests.sh orchestrates them.",
                "Test inspection passes with non-placeholder test files and a usable unified runner.",
            )
        )

    fake_result = run_script(scripts_dir / "check_fake_impl.py", root)
    if fake_result.returncode != 0:
        fake_issue = make_issue(
            "fake-implementation-risk",
            "major",
            "3.2.2",
            fake_result.stdout or fake_result.stderr,
            "Replace placeholder logic, static success responses, or hardcoded data flows with real behavior.",
            "The fake-implementation scan no longer reports suspicious markers, or flagged lines are justified and removed from core flows.",
        )
        payload["non_blocking_issues"].append(fake_issue)
        payload["suspected_fake_implementations"].append(fake_issue)

    runtime_cwd = repo_root or root
    if args.runtime_command:
        runtime_result = subprocess.run(
            args.runtime_command,
            capture_output=True,
            text=True,
            cwd=runtime_cwd,
            shell=True,
            timeout=args.runtime_timeout_seconds,
        )
        runtime_output = (runtime_result.stdout or runtime_result.stderr).strip() or "Runtime command produced no output."
        payload["runtime_evidence"].append(runtime_output)
        if runtime_result.returncode != 0:
            payload["blocking_issues"].append(
                make_issue(
                    "runtime-verification-failed",
                    "blocking",
                    "3.1.1 / 3.7.1",
                    runtime_output,
                    "Fix the runtime command and any startup errors until the environment boots cleanly.",
                    "The runtime command exits with code 0 and the service remains available for verification.",
                )
            )
    else:
        payload["runtime_evidence"].append("Runtime command not executed. Manual or automated runtime verification is still required.")
        payload["non_blocking_issues"].append(
            make_issue(
                "runtime-verification-missing",
                "major",
                "3.1.1",
                "run_acceptance.py was executed without --runtime-command.",
                "Execute a real runtime verification command, usually docker compose startup from repo/, and capture the output.",
                "Runtime evidence is attached and no startup failure is reported.",
            )
        )

    if payload["blocking_issues"]:
        payload["overall_verdict"] = "FAIL"
        payload["submit_readiness"] = "Not ready to submit"
    elif payload["non_blocking_issues"]:
        payload["overall_verdict"] = "REWORK"
        payload["submit_readiness"] = "Needs rework before submission"

    payload["repair_priority"] = [
        issue["issue_id"] for issue in payload["blocking_issues"] + payload["non_blocking_issues"]
    ]

    output_json = Path(args.output_json) if args.output_json else None
    output_md = Path(args.output_md) if args.output_md else None
    if output_json is not None:
        output_json.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    else:
        print(json.dumps(payload, ensure_ascii=False, indent=2))

    if output_md is not None:
        maybe_render_markdown(payload, output_json, output_md, scripts_dir / "generate_qa_report.py")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
