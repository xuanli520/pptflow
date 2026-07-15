#!/usr/bin/env bash
set -euo pipefail

# Generate the final immutable Standard-authoring deployment lock from a clean
# committed source snapshot. This script deliberately does not inspect model
# credentials, endpoints, or arbitrary environment variables. The caller must
# provide every host/model identity as an explicit flag accepted by the Go
# generator, including --build-version, --lock-version, --git-executable,
# --codex-node, --codex-launcher, --codex-home, and --codex-model-version.

die() {
  echo "generate-standard-authoring-lock: $*" >&2
  exit 1
}

root="$(CDPATH= cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -n "$(git -C "$root" status --porcelain=v1 --untracked-files=all)" ]]; then
  die "a clean committed source tree is required"
fi

catalog="$root/deployments/standard-authoring/operation-catalog.v1.json"
assets="$root/deployments/standard-authoring/contract-assets.v1.json"
contract_root="$root/deployments/standard-authoring"
output="$root/deployments/standard-authoring/operation-catalog.lock.json"

for file in "$catalog" "$assets"; do
  if [[ ! -f "$file" || -L "$file" ]]; then
    die "required regular deployment file is unavailable: $file"
  fi
done
if [[ -e "$output" || -L "$output" ]]; then
  die "refusing to replace existing lock: $output"
fi

exec env GOFLAGS= go run -mod=readonly ./tools/standard-authoring-lock-build \
  --source-root "$root" \
  --catalog "$catalog" \
  --asset-manifest "$assets" \
  --contract-root "$contract_root" \
  --output "$output" \
  "$@"
