#!/usr/bin/env python3
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
non_ascii = []
for path in (root / "repo").rglob("*"):
    if path.is_file() and path.suffix.lower() in {".md", ".txt", ".py", ".js", ".ts", ".tsx", ".go"}:
        text = path.read_text(encoding="utf-8", errors="ignore")
        if any(ord(ch) > 127 for ch in text):
            non_ascii.append(str(path.relative_to(root)))
print(json.dumps({"non_ascii_files": non_ascii[:200]}, ensure_ascii=False, indent=2))
sys.exit(0)
