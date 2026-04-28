#!/usr/bin/env python3
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
bad_names = {".venv", "venv", "node_modules", ".pytest_cache", "__pycache__"}
matches = [str(path.relative_to(root)) for path in root.rglob("*") if path.name in bad_names]
print(json.dumps({"local_dependency_paths": matches}, ensure_ascii=False, indent=2))
sys.exit(0)
