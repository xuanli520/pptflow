#!/usr/bin/env python3
"""
clean_original_sessions.py

Purpose:
- Clean error events from raw Claude sessions (cleaning only, no message conversion).
- Output the cleaned raw directory to `original_sessions/<project_id>/`.
- Optionally also output `original_sessions/<project_id>.zip`.

Command:
- `python clean_original_sessions.py [-r ROOT] [-o OUTPUT_DIR] [--project-id PROJECT_ID] [--overwrite] [--dry-run] [--zip]`

Arguments:
- `-r, --root`: Primary input directory. Scanned first; fallback directory supplements any missing sessions.
- `-o, --output-dir`: Output root directory. Default: `current_working_directory/original_sessions`.
- `--project-id`: Output project directory name. Default: fallback directory name (`-<path-encoded>`).
- `--overwrite`: Allow overwriting if the target directory or zip already exists.
- `--dry-run`: Only print location and execution plan; do not write files.
- `--zip`: Also generate a zip archive; omitting this flag keeps only the cleaned directory.

Fallback location when `-r` is not provided:
- Base path uses the current working directory `Path.cwd()`.
- Fallback directory: `~/.claude/projects/-<path-encoded>`.
- Path encoding rule: split the absolute base path by `/`, join with `-`, and prepend `-`.
- Windows adaptation:
  - Detected automatically via `os.name == "nt"`.
  - Fallback path encoding handles Windows path separators and drive letters, producing `C--Users-wxy-...` form.
  - Example: `C:\\Users\\wxy\\Desktop\\TASK-123` -> `C:\\Users\\wxy\\.claude\\projects\\C--Users-wxy-Desktop-TASK-123`.
  - If `--zip` is passed, the default zip filename is also `C--Users-wxy-Desktop-TASK-123.zip`.

Execution order:
1) Locate session sources (primary + fallback).
2) Clean `.jsonl` error events and copy directory to the output path.
3) If `--zip` is passed, compress the output directory into a zip.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import zipfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass
class CopyStats:
    copied_files: int = 0
    filtered_jsonl_files: int = 0
    passthrough_jsonl_files: int = 0
    total_events: int = 0
    removed_events: int = 0


@dataclass
class ZipStats:
    zip_path: Path
    archived_files: int
    zip_size_bytes: int


@dataclass
class SessionSource:
    session_id: str
    session_jsonl: Path
    session_dir: Path
    source_root: Path
    has_subagents: bool


_API_ERROR_WITH_STATUS_RE = re.compile(r"\bapi[\s_-]?error\b\s*[:：]?\s*(4\d{2}|5\d{2})\b", re.IGNORECASE)
_REMOTE_ERROR_HINTS = (
    "request id",
    "failed to authenticate",
    "token has no access",
    "no access to model",
    "channel has been disabled",
    "bad response status code",
    "oneapi",
    "new-api",
    "no permission to access model",
)
_WINDOWS_INVALID_CHARS_RE = re.compile(r'[<>:"/\\|?*\x00-\x1F]')
_WINDOWS_RESERVED_NAMES = {
    "CON", "PRN", "AUX", "NUL",
    "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
    "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
}
_CHINESE_RE = re.compile(r"[\u4e00-\u9fff]")
_STRIP_CHINESE_TEXT_SUFFIXES = {".json", ".jsonl", ".md", ".txt", ".yaml", ".yml"}
_IGNORED_DIR_NAMES = {
    ".codex",
    ".pytest_cache",
    ".venv",
    "venv",
    ".net",
    ".vscode",
    "node_modules",
    "__pycache__",
    "__pycahce__",
}


def _contains_remote_api_error_text(value: Any) -> bool:
    if isinstance(value, str):
        t = value.lower()
        if "new_api_error" in t or "new_api_errer" in t:
            return True
        if "api error" not in t and "api_error" not in t:
            return False
        if _API_ERROR_WITH_STATUS_RE.search(t):
            return True
        return any(hint in t for hint in _REMOTE_ERROR_HINTS)
    if isinstance(value, dict):
        return any(_contains_remote_api_error_text(v) for v in value.values())
    if isinstance(value, list):
        return any(_contains_remote_api_error_text(v) for v in value)
    return False


def _strip_chinese_from_string(value: str) -> str:
    return _CHINESE_RE.sub("", value)


def _strip_chinese_from_json_value(value: Any) -> Any:
    if isinstance(value, str):
        return _strip_chinese_from_string(value)
    if isinstance(value, list):
        return [_strip_chinese_from_json_value(item) for item in value]
    if isinstance(value, dict):
        return {key: _strip_chinese_from_json_value(item) for key, item in value.items()}
    return value


def _is_agent_tool_result_api_error_event(obj: dict[str, Any]) -> bool:
    if obj.get("type") != "user":
        return False

    message = obj.get("message")
    if not isinstance(message, dict) or message.get("role") != "user":
        return False

    content = message.get("content")
    if not isinstance(content, list):
        return False

    has_tool_result = any(
        isinstance(item, dict) and item.get("type") == "tool_result"
        for item in content
    )
    if not has_tool_result:
        return False

    tool_use_result = obj.get("toolUseResult")
    if not isinstance(tool_use_result, dict):
        return False

    # Only clean remote model call errors in Agent sub-agent results, to avoid
    # accidentally deleting normal tool/API validation output
    if not isinstance(tool_use_result.get("agentId"), str):
        return False

    for item in content:
        if not isinstance(item, dict) or item.get("type") != "tool_result":
            continue
        if _contains_remote_api_error_text(item.get("content")):
            return True

    return _contains_remote_api_error_text(tool_use_result.get("content"))


def _is_api_error_event(obj: dict[str, Any]) -> bool:
    # 1) Claude native API error event
    if obj.get("type") == "system" and obj.get("subtype") == "api_error":
        return True

    # 2) synthetic error message (contains error / isApiErrorMessage)
    if obj.get("isApiErrorMessage") is True:
        return True

    # 3) Agent sub-agent call to remote model failed (e.g. Failed to authenticate / new_api_error)
    if _is_agent_tool_result_api_error_event(obj):
        return True

    return False


def _sanitize_path_part_for_windows(part: str) -> str:
    # Drive letter segment example: C: -> C
    if re.fullmatch(r"[A-Za-z]:", part):
        part = part[0]

    part = _WINDOWS_INVALID_CHARS_RE.sub("-", part).strip(" .")
    if not part:
        part = "_"

    if part.upper() in _WINDOWS_RESERVED_NAMES:
        part = f"{part}_"
    return part


def _encode_windows_abs_path(input_path: Path) -> str:
    """
    Windows encoding rules (consistent with user convention):
    - Path separators / and \\ are both replaced with -
    - Drive letter colon : is replaced with -
    - Example: C:\\Users\\wxy\\Desktop\\TASK-123 -> C--Users-wxy-Desktop-TASK-123
    """
    raw = str(input_path.resolve())
    encoded = raw.replace("\\", "-").replace("/", "-")
    encoded = encoded.replace(":", "-")
    encoded = _WINDOWS_INVALID_CHARS_RE.sub("-", encoded)
    encoded = encoded.strip(" .")
    if not encoded:
        encoded = "_"
    if encoded.upper() in _WINDOWS_RESERVED_NAMES:
        encoded = f"{encoded}_"
    return encoded


def encode_abs_path_for_claude_projects(input_path: Path) -> str:
    if os.name == "nt":
        return _encode_windows_abs_path(input_path)
    abs_posix = input_path.resolve().as_posix()
    parts = [p for p in abs_posix.split("/") if p]
    return "-" + "-".join(parts) if parts else "-"


def fallback_root_for_input(input_root: Path) -> Path:
    encoded = encode_abs_path_for_claude_projects(input_root)
    return Path.home() / ".claude" / "projects" / encoded


def _candidate_jsonl_files(root: Path) -> list[Path]:
    # Filter by suffix.lower() uniformly to support case-insensitive Windows filesystems
    files = [p for p in root.iterdir() if p.is_file() and p.suffix.lower() == ".jsonl"]
    return sorted(files, key=lambda p: p.name.lower())


def list_session_sources(root: Path | None) -> dict[str, SessionSource]:
    if root is None or not root.exists() or not root.is_dir():
        return {}

    out: dict[str, SessionSource] = {}
    for session_jsonl in _candidate_jsonl_files(root):
        sid = session_jsonl.stem
        session_dir = root / sid
        subagents_dir = session_dir / "subagents"
        has_subagents = subagents_dir.is_dir() and any(subagents_dir.glob("agent-*.jsonl"))
        out[sid] = SessionSource(
            session_id=sid,
            session_jsonl=session_jsonl,
            session_dir=session_dir,
            source_root=root,
            has_subagents=bool(has_subagents),
        )
    return out


def choose_session_source(primary: SessionSource | None, fallback: SessionSource | None) -> SessionSource | None:
    """
    Selection priority:
    1) primary with subagents
    2) fallback with subagents
    3) primary (main session only)
    4) fallback (main session only)
    """
    if primary and primary.has_subagents:
        return primary
    if fallback and fallback.has_subagents:
        return fallback
    if primary:
        return primary
    if fallback:
        return fallback
    return None


def resolve_session_sources(
    root_arg: Path | None,
) -> tuple[list[SessionSource], Path | None, Path, bool]:
    """
    Returns:
    - Selected session list
    - primary_root
    - fallback_root
    - Whether fallback was used as the source for at least one session
    """
    primary_root = root_arg.resolve() if root_arg else None
    base_for_fallback = primary_root if primary_root else Path.cwd().resolve()
    fallback_root = fallback_root_for_input(base_for_fallback)

    primary_map = list_session_sources(primary_root) if primary_root else {}
    fallback_map = list_session_sources(fallback_root)

    all_ids = sorted(set(primary_map.keys()) | set(fallback_map.keys()))
    chosen: list[SessionSource] = []
    used_fallback = False

    for sid in all_ids:
        source = choose_session_source(primary_map.get(sid), fallback_map.get(sid))
        if source is None:
            continue
        if source.source_root == fallback_root:
            used_fallback = True
        chosen.append(source)

    return chosen, primary_root, fallback_root, used_fallback


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Clean Claude origin session files by removing API-error events only, "
            "then copy cleaned project folder into ./original_sessions"
        )
    )
    parser.add_argument(
        "-r",
        "--root",
        type=Path,
        default=None,
        help="Input directory (optional). If omitted, only the fallback directory is used.",
    )
    parser.add_argument(
        "-o",
        "--output-dir",
        type=Path,
        default=None,
        help="Output root directory (default: current_directory/original_sessions).",
    )
    parser.add_argument(
        "--project-id",
        type=str,
        default=None,
        help="Output project directory name (default: fallback directory name).",
    )
    parser.add_argument(
        "--overwrite",
        action="store_true",
        help="Delete and recreate output directory if it already exists.",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Only print location and plan; do not perform any copy or write operations.",
    )
    parser.add_argument(
        "--zip",
        action="store_true",
        help="Also output a zip archive (not compressed by default).",
    )
    parser.add_argument(
        "--strip-chinese",
        action="store_true",
        help="Remove Chinese characters from copied text payloads and JSONL event content.",
    )
    return parser.parse_args()


def _copy_filtered_jsonl(src: Path, dst: Path, *, strip_chinese: bool = False) -> tuple[bool, int, int]:
    """
    Returns: (whether event-filtered write succeeded, total_events, removed_events)
    - True: filtered by event and written
    - False: not filtered (e.g. unexpected format); caller should fall back to plain copy2
    """
    total_events = 0
    removed_events = 0

    dst.parent.mkdir(parents=True, exist_ok=True)
    tmp_dst = dst.with_suffix(dst.suffix + ".tmpclean")
    if tmp_dst.exists():
        tmp_dst.unlink()

    parse_ok = True
    try:
        with src.open("r", encoding="utf-8") as rf, tmp_dst.open("w", encoding="utf-8") as wf:
            for line in rf:
                line = line.strip()
                if not line:
                    continue
                total_events += 1
                try:
                    obj = json.loads(line)
                except json.JSONDecodeError:
                    # Non-standard Claude event jsonl; let the caller fall back to copy2
                    parse_ok = False
                    break
                if not isinstance(obj, dict):
                    parse_ok = False
                    break
                if _is_api_error_event(obj):
                    removed_events += 1
                    continue
                if strip_chinese:
                    obj = _strip_chinese_from_json_value(obj)
                wf.write(json.dumps(obj, ensure_ascii=False, separators=(",", ":")))
                wf.write("\n")
    except Exception:
        if tmp_dst.exists():
            tmp_dst.unlink()
        raise

    if not parse_ok:
        if tmp_dst.exists():
            tmp_dst.unlink()
        return False, 0, 0

    tmp_dst.replace(dst)

    return True, total_events, removed_events


def _copy_tree_with_jsonl_filter(
    src_root: Path,
    dst_root: Path,
    stats: CopyStats,
    *,
    strip_chinese: bool = False,
) -> None:
    if not src_root.exists():
        return

    for path in src_root.rglob("*"):
        rel = path.relative_to(src_root)
        if any(part.lower() in _IGNORED_DIR_NAMES for part in rel.parts):
            continue
        target = dst_root / rel

        if path.is_dir():
            target.mkdir(parents=True, exist_ok=True)
            continue

        if not path.is_file():
            continue

        target.parent.mkdir(parents=True, exist_ok=True)

        if path.suffix.lower() == ".jsonl":
            ok, total_events, removed_events = _copy_filtered_jsonl(path, target, strip_chinese=strip_chinese)
            if ok:
                stats.filtered_jsonl_files += 1
                stats.total_events += total_events
                stats.removed_events += removed_events
            else:
                if strip_chinese:
                    try:
                        text = path.read_text(encoding="utf-8")
                    except UnicodeDecodeError:
                        shutil.copy2(path, target)
                    else:
                        target.write_text(_strip_chinese_from_string(text), encoding="utf-8")
                else:
                    shutil.copy2(path, target)
                stats.passthrough_jsonl_files += 1
        elif strip_chinese and path.suffix.lower() in _STRIP_CHINESE_TEXT_SUFFIXES:
            try:
                text = path.read_text(encoding="utf-8")
            except UnicodeDecodeError:
                shutil.copy2(path, target)
            else:
                target.write_text(_strip_chinese_from_string(text), encoding="utf-8")
        else:
            shutil.copy2(path, target)

        stats.copied_files += 1


def _copy_main_and_session_dirs(
    sessions: list[SessionSource],
    dst_project_root: Path,
    stats: CopyStats,
    *,
    strip_chinese: bool = False,
) -> None:
    for source in sessions:
        dst_main_jsonl = dst_project_root / source.session_jsonl.name

        ok, total_events, removed_events = _copy_filtered_jsonl(
            source.session_jsonl,
            dst_main_jsonl,
            strip_chinese=strip_chinese,
        )
        if ok:
            stats.filtered_jsonl_files += 1
            stats.total_events += total_events
            stats.removed_events += removed_events
        else:
            if strip_chinese:
                try:
                    text = source.session_jsonl.read_text(encoding="utf-8")
                except UnicodeDecodeError:
                    shutil.copy2(source.session_jsonl, dst_main_jsonl)
                else:
                    dst_main_jsonl.write_text(_strip_chinese_from_string(text), encoding="utf-8")
            else:
                shutil.copy2(source.session_jsonl, dst_main_jsonl)
            stats.passthrough_jsonl_files += 1
        stats.copied_files += 1

        if source.session_dir.exists() and source.session_dir.is_dir():
            dst_session_dir = dst_project_root / source.session_dir.name
            if dst_session_dir.exists():
                shutil.rmtree(dst_session_dir)
            _copy_tree_with_jsonl_filter(
                source.session_dir,
                dst_session_dir,
                stats,
                strip_chinese=strip_chinese,
            )


def _zip_project_dir(project_dir: Path, zip_path: Path, *, overwrite: bool) -> ZipStats:
    if zip_path.exists():
        if not overwrite:
            raise FileExistsError(f"zip already exists: {zip_path}")
        zip_path.unlink()

    archived_files = 0
    root_name = project_dir.name

    with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_DEFLATED) as zf:
        for path in sorted(project_dir.rglob("*")):
            if not path.is_file():
                continue
            arcname = (Path(root_name) / path.relative_to(project_dir)).as_posix()
            zf.write(path, arcname)
            archived_files += 1

    zip_size = zip_path.stat().st_size if zip_path.exists() else 0
    return ZipStats(zip_path=zip_path, archived_files=archived_files, zip_size_bytes=zip_size)


def _is_subpath(path: Path, parent: Path) -> bool:
    try:
        # Normalize case on Windows to avoid false mismatches between C:\\A and c:\\a
        path_resolved = Path(os.path.normcase(str(path.resolve())))
        parent_resolved = Path(os.path.normcase(str(parent.resolve())))
        path_resolved.relative_to(parent_resolved)
        return True
    except ValueError:
        return False


def main() -> int:
    args = parse_args()

    root_arg = args.root.expanduser() if args.root else None
    sessions, primary_root, fallback_root, used_fallback = resolve_session_sources(
        root_arg=root_arg,
    )

    if not sessions:
        if primary_root:
            print(f"[WARN] no session jsonl found under: {primary_root}")
        print(f"[WARN] no session jsonl found under fallback: {fallback_root}")
        return 1

    # Location phase
    source_roots = sorted({str(s.source_root) for s in sessions})
    # When --root is provided, prefer primary_root as base; supplement with session sources as overlay
    preferred_base_root = primary_root if (primary_root and primary_root.exists()) else fallback_root
    if preferred_base_root is None:
        preferred_base_root = fallback_root

    output_base = (
        args.output_dir.expanduser().resolve()
        if args.output_dir
        else (Path.cwd() / "original_sessions")
    )

    project_id = args.project_id or fallback_root.name
    dst_project_root = output_base / project_id
    zip_output_path = output_base / f"{project_id}.zip"

    total_steps = 4 if args.zip else 3
    print(f"[STEP 1/{total_steps}] locate")
    print(f"  primary_root : {primary_root}")
    print(f"  fallback_root: {fallback_root}")
    print(f"  used_fallback: {used_fallback}")
    print(f"  source_roots : {source_roots}")
    print(f"  sessions     : {len(sessions)}")
    print(f"  output_dir   : {dst_project_root}")
    if args.zip:
        print(f"  output_zip   : {zip_output_path}")

    if args.dry_run:
        print("[DRY-RUN] stop before copy")
        return 0

    # Guard against output dir being inside a source dir, which would cause recursive copy / size explosion
    source_root_paths = {s.source_root.resolve() for s in sessions}
    for src_root in source_root_paths:
        if _is_subpath(output_base, src_root):
            print(f"[ERROR] output dir is inside source root: {output_base}")
            print(f"        source root: {src_root}")
            print("        please set --output-dir outside source roots")
            return 2

    output_base.mkdir(parents=True, exist_ok=True)

    if dst_project_root.exists():
        if not args.overwrite:
            print(f"[ERROR] output already exists: {dst_project_root}")
            print("        use --overwrite to replace it")
            return 2
        shutil.rmtree(dst_project_root)

    if args.zip and zip_output_path.exists() and not args.overwrite:
        print(f"[ERROR] zip already exists: {zip_output_path}")
        print("        use --overwrite to replace it")
        return 2

    # Delete + copy phase (copy full directory first, then overlay sessions from alternate sources)
    print(f"[STEP 2/{total_steps}] remove error events while copying")
    stats = CopyStats()

    if preferred_base_root and preferred_base_root.exists() and preferred_base_root.is_dir():
        _copy_tree_with_jsonl_filter(
            preferred_base_root,
            dst_project_root,
            stats,
            strip_chinese=args.strip_chinese,
        )

    # Only overlay sessions not from preferred_base_root to avoid duplicate copying of same-source sessions
    overlay_sessions = [s for s in sessions if s.source_root != preferred_base_root]
    if overlay_sessions:
        _copy_main_and_session_dirs(
            overlay_sessions,
            dst_project_root,
            stats,
            strip_chinese=args.strip_chinese,
        )

    zip_stats: ZipStats | None = None
    if args.zip:
        print(f"[STEP 3/{total_steps}] zip cleaned project folder")
        zip_stats = _zip_project_dir(dst_project_root, zip_output_path, overwrite=args.overwrite)

    print(f"[STEP {total_steps}/{total_steps}] done")
    print(f"  copied_files          : {stats.copied_files}")
    print(f"  filtered_jsonl_files  : {stats.filtered_jsonl_files}")
    print(f"  passthrough_jsonl     : {stats.passthrough_jsonl_files}")
    print(f"  total_events_seen     : {stats.total_events}")
    print(f"  removed_error_events  : {stats.removed_events}")
    print(f"  output_project_folder : {dst_project_root}")
    if zip_stats is not None:
        print(f"  output_zip_file       : {zip_stats.zip_path}")
        print(f"  zip_archived_files    : {zip_stats.archived_files}")
        print(f"  zip_size_bytes        : {zip_stats.zip_size_bytes}")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
