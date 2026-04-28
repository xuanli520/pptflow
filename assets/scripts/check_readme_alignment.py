#!/usr/bin/env python3
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
readmes = list((root / "repo").glob("README*"))
print(json.dumps({"readme_count": len(readmes), "manual_review_required": True}, ensure_ascii=False, indent=2))
sys.exit(0)
