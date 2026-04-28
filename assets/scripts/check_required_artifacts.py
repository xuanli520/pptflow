#!/usr/bin/env python3
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
checks = {
    "README": any((root / "repo").glob("README*")),
    "docs": (root / "docs").is_dir(),
    "unit_tests": (root / "unit_tests").is_dir(),
    "api_tests": (root / "API_tests").is_dir() or (root / "api_tests").is_dir(),
    "run_tests": any((root / "repo").glob("run_tests.*")),
}
print(json.dumps(checks, ensure_ascii=False, indent=2))
sys.exit(0)
