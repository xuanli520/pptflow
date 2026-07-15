#!/usr/bin/env bash
set -euo pipefail

# Generate the immutable CodeEdge Phase-1 parent deployment lock from a clean
# committed source snapshot. The caller must provide explicit absolute Git and
# Docker executable paths. This script never reads model endpoints, model
# credentials, or arbitrary provider configuration from the environment.

die() {
  echo "generate-codeedge-phase1-lock: $*" >&2
  exit 1
}

: "${HARBOR_FACTORY_GIT_EXECUTABLE:?set HARBOR_FACTORY_GIT_EXECUTABLE to the absolute Git executable}"
: "${HARBOR_FACTORY_DOCKER_EXECUTABLE:?set HARBOR_FACTORY_DOCKER_EXECUTABLE to the absolute Docker executable}"

git_executable="$HARBOR_FACTORY_GIT_EXECUTABLE"
docker_executable="$HARBOR_FACTORY_DOCKER_EXECUTABLE"
if [[ "$git_executable" != /* || "$docker_executable" != /* ]]; then
  die "HARBOR_FACTORY_GIT_EXECUTABLE and HARBOR_FACTORY_DOCKER_EXECUTABLE must be absolute paths"
fi

root="$(CDPATH= cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -n "$("$git_executable" -C "$root" status --porcelain=v1 --untracked-files=all)" ]]; then
  die "a clean committed source tree is required"
fi

catalog="$root/deployments/codeedge-phase1/operation-catalog.v1.json"
profile="$root/deployments/codeedge-phase1/execution-profile.v1.json"
preflight="$root/deployments/codeedge-phase1/preflight-profile.v1.json"
policy="$root/deployments/codeedge-phase1/final-compliance-policy.v1.json"
output="$root/deployments/codeedge-phase1/operation-catalog.lock.json"

for file in "$catalog" "$profile" "$preflight" "$policy"; do
  if [[ ! -f "$file" || -L "$file" ]]; then
    die "required regular deployment file is unavailable: $file"
  fi
done
if [[ -e "$output" || -L "$output" ]]; then
  die "refusing to replace existing lock: $output"
fi

exec env GOFLAGS= go run -mod=readonly ./tools/codeedge-phase1-lock-build \
  --source-root "$root" \
  --catalog "$catalog" \
  --execution-profile "$profile" \
  --preflight-profile "$preflight" \
  --final-compliance-policy "$policy" \
  --output "$output" \
  --git-executable "$git_executable" \
  --docker-executable "$docker_executable" \
  "$@"
