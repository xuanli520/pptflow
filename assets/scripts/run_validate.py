#!/usr/bin/env python3
"""
Auto-update wrapper for validate_package.py

Before each invocation, this script checks whether validate_package.py in the
remote repository has changed:
- if updated, sync it locally and then execute it
- if unchanged, execute the local copy directly
All arguments are forwarded to validate_package.py unchanged.

Usage is identical to validate_package.py:
  python run_validate.py /path/to/TASK-001
  python run_validate.py TASK-001 --repair
  python run_validate.py TASK-001 --convert-legacy
"""

import hashlib
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from urllib.parse import quote

import requests
import urllib3

urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

GITLAB_URL = "https://gitlab.mindflow.com.cn"
PROJECT_ID = "5736"
GITLAB_TOKEN = "glpat-uRmhPUuYDBKhvFmLMmKA1W86MQp1OjIwCA.01.0y0regzdt"
BRANCH = "main"

REMOTE_FILE_PATH = "validate_package.py"
LOCAL_DIR = Path("/opt/devenv")
LOCAL_FILE = LOCAL_DIR / "validate_package.py"


def gitlab_get(url, params=None, raw=False):
    headers = {"PRIVATE-TOKEN": GITLAB_TOKEN}
    attempt = 0
    while True:
        attempt += 1
        try:
            resp = requests.get(
                url, headers=headers, params=params, timeout=30, verify=False
            )
            resp.raise_for_status()
            return resp.content if raw else resp.json()
        except Exception as e:
            print(f"  [Retry] Request failed on attempt {attempt}, retrying in 3 seconds... ({e})")
            time.sleep(3)


def get_remote_sha256():
    encoded = quote(REMOTE_FILE_PATH, safe="")
    url = f"{GITLAB_URL}/api/v4/projects/{quote(str(PROJECT_ID), safe='')}/repository/files/{encoded}"
    params = {"ref": BRANCH}
    meta = gitlab_get(url, params)
    return meta.get("content_sha256", "")


def get_local_sha256():
    if not LOCAL_FILE.is_file():
        return ""
    h = hashlib.sha256()
    h.update(LOCAL_FILE.read_bytes())
    return h.hexdigest()


def download_remote_file():
    encoded = quote(REMOTE_FILE_PATH, safe="")
    url = f"{GITLAB_URL}/api/v4/projects/{quote(str(PROJECT_ID), safe='')}/repository/files/{encoded}/raw"
    params = {"ref": BRANCH}
    return gitlab_get(url, params, raw=True)


def _get_real_user():
    return os.environ.get("SUDO_USER") or os.getlogin()


def _needs_sudo():
    return os.getuid() != 0


def _sudo_write(content, dest):
    real_user = _get_real_user()
    with tempfile.NamedTemporaryFile(delete=False) as tmp:
        tmp.write(content)
        tmp_path = tmp.name
    try:
        subprocess.run(["sudo", "mkdir", "-p", str(dest.parent)], check=True)
        subprocess.run(["sudo", "chown", f"{real_user}:{real_user}", str(dest.parent)], check=True)
        subprocess.run(["sudo", "cp", tmp_path, str(dest)], check=True)
        subprocess.run(["sudo", "chown", f"{real_user}:{real_user}", str(dest)], check=True)
    finally:
        os.unlink(tmp_path)


def sync_if_needed():
    print("[AutoUpdate] Checking whether validate_package.py has updates...")

    remote_sha = get_remote_sha256()
    local_sha = get_local_sha256()

    if remote_sha and remote_sha == local_sha:
        print("[AutoUpdate] Local copy is already up to date")
        return

    if not remote_sha:
        print("[AutoUpdate] Could not fetch remote version metadata, skipping update check")
        return

    reason = "Local file is missing" if not local_sha else "Remote update detected"
    print(f"[AutoUpdate] {reason}, syncing now...")

    content = download_remote_file()
    if _needs_sudo():
        _sudo_write(content, LOCAL_FILE)
    else:
        LOCAL_DIR.mkdir(parents=True, exist_ok=True)
        LOCAL_FILE.write_bytes(content)
    print(f"[AutoUpdate] Updated: {LOCAL_FILE}")


def main():
    try:
        sync_if_needed()
    except Exception as e:
        print(f"[AutoUpdate] Update check failed ({e}), continuing with the local version")

    print()

    if not LOCAL_FILE.is_file():
        print(f"Error: {LOCAL_FILE} does not exist and could not be fetched from remote")
        return 1

    result = subprocess.run(
        [sys.executable, str(LOCAL_FILE)] + sys.argv[1:],
    )
    return result.returncode


if __name__ == "__main__":
    sys.exit(main())
