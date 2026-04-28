#!/usr/bin/env python3
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
required = ["docs", "repo", "original_sessions", "metadata.json"]
missing = [name for name in required if not (root / name).exists()]
print(json.dumps({"root": str(root), "missing": missing, "ok": not missing}, ensure_ascii=False, indent=2))
sys.exit(0 if not missing else 1)
