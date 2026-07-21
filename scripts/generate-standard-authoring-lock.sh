#!/usr/bin/env bash
set -euo pipefail

# Generate the final immutable Standard-authoring deployment lock from a clean
# committed source snapshot. This script deliberately does not inspect model
# credentials, endpoints, or arbitrary environment variables. The caller must
# provide every host/model identity and the source-controlled complete
# --execution-profile accepted by the Go generator, including --build-version,
# --lock-version, --git-executable, --ssh-executable, --ssh-wrapper-shell,
# --codex-node, --codex-launcher, --codex-home, and --codex-model-version.
# The approved model and reasoning effort are source-controlled in the catalog;
# ambient Codex configuration is never used as a substitute.

die() {
  echo "generate-standard-authoring-lock: $*" >&2
  exit 1
}

root="$(CDPATH= cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -n "$(git -C "$root" status --porcelain=v1 --untracked-files=all)" ]]; then
  die "a clean committed source tree is required"
fi

catalog="$root/deployments/standard-authoring-1.8/operation-catalog.v1.json"
assets="$root/deployments/standard-authoring-1.8/contract-assets.v1.json"
profile="$root/deployments/standard-authoring-1.8/execution-profile.v1.json"
contract_root="$root/deployments/standard-authoring-1.8"
ssh_known_hosts="$contract_root/ssh/known_hosts"
output="$root/deployments/standard-authoring-1.8/operation-catalog.lock.json"

for file in "$catalog" "$assets" "$profile" "$ssh_known_hosts"; do
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
  --execution-profile "$profile" \
  --contract-root "$contract_root" \
  --output "$output" \
  "$@" \
  --ssh-known-hosts "$ssh_known_hosts"
