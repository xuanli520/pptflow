#!/usr/bin/env python3
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
metadata = root / "metadata.json"
result = {"root": str(root), "metadata_readable": metadata.exists(), "prompt_present": False}
if metadata.exists():
    try:
        data = json.loads(metadata.read_text(encoding="utf-8"))
        result["prompt_present"] = bool(data.get("prompt"))
    except Exception as exc:
        result["metadata_error"] = str(exc)
print(json.dumps(result, ensure_ascii=False, indent=2))
sys.exit(0 if result["metadata_readable"] else 1)
