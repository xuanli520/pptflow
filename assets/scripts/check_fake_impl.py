#!/usr/bin/env python3
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
pattern = re.compile(r"\b(TODO|FIXME|mock|stub|fake)\b", re.IGNORECASE)
matches = []
for path in (root / "repo").rglob("*"):
    if path.is_file() and path.suffix.lower() in {".py", ".js", ".ts", ".tsx", ".go", ".java", ".md"}:
        try:
            for index, line in enumerate(path.read_text(encoding="utf-8", errors="ignore").splitlines(), 1):
                if pattern.search(line):
                    matches.append({"path": str(path.relative_to(root)), "line": index})
        except OSError:
            pass
print(json.dumps({"matches": matches[:200]}, ensure_ascii=False, indent=2))
sys.exit(0)
