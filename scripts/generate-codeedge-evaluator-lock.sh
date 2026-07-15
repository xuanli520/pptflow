#!/usr/bin/env bash
set -euo pipefail

# Generate the immutable CodeEdge evaluator-child deployment lock from one
# clean committed source snapshot. Runtime paths are supplied explicitly; the
# three evaluator environment values are read only by the Go generator to
# compare their fingerprints with the source-controlled catalog. This script
# never writes or prints endpoint or credential values.

die() {
  echo "generate-codeedge-evaluator-lock: $*" >&2
  exit 1
}

if (( $# != 0 )); then
  die "this closed production invocation accepts no positional arguments"
fi

: "${HARBOR_FACTORY_GIT_EXECUTABLE:?set HARBOR_FACTORY_GIT_EXECUTABLE to the absolute Git executable}"
: "${HARBOR_FACTORY_HARBOR_LAUNCHER:?set HARBOR_FACTORY_HARBOR_LAUNCHER to the absolute Harbor launcher}"
: "${HARBOR_FACTORY_PYTHON_INTERPRETER:?set HARBOR_FACTORY_PYTHON_INTERPRETER to the absolute Harbor Python interpreter}"
: "${HARBOR_FACTORY_HARBOR_PYTHON_SOURCE_TREE:?set HARBOR_FACTORY_HARBOR_PYTHON_SOURCE_TREE to the absolute Harbor Python package directory}"
: "${HARBOR_FACTORY_DOCKER_EXECUTABLE:?set HARBOR_FACTORY_DOCKER_EXECUTABLE to the absolute Docker executable}"
: "${HARBOR_FACTORY_BUILD_VERSION:?set HARBOR_FACTORY_BUILD_VERSION to the immutable Harbor Factory build version}"
: "${HARBOR_FACTORY_CODEEDGE_EVALUATOR_LOCK_VERSION:?set HARBOR_FACTORY_CODEEDGE_EVALUATOR_LOCK_VERSION to the immutable evaluator lock version}"
: "${QWEN_HARBOR_BASE_URL:?set QWEN_HARBOR_BASE_URL in the environment}"
: "${OPUS_HARBOR_BASE_URL:?set OPUS_HARBOR_BASE_URL in the environment}"
: "${ANTHROPIC_AUTH_TOKEN:?set ANTHROPIC_AUTH_TOKEN in the environment}"

git_executable="$HARBOR_FACTORY_GIT_EXECUTABLE"
harbor_launcher="$HARBOR_FACTORY_HARBOR_LAUNCHER"
python_interpreter="$HARBOR_FACTORY_PYTHON_INTERPRETER"
python_source_tree="$HARBOR_FACTORY_HARBOR_PYTHON_SOURCE_TREE"
docker_executable="$HARBOR_FACTORY_DOCKER_EXECUTABLE"

for value in "$git_executable" "$harbor_launcher" "$python_interpreter" "$python_source_tree" "$docker_executable"; do
  if [[ "$value" != /* ]]; then
    die "all runtime paths must be absolute"
  fi
done

root="$(CDPATH= cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -n "$("$git_executable" -C "$root" status --porcelain=v1 --untracked-files=all)" ]]; then
  die "a clean committed source tree is required"
fi

catalog="$root/deployments/codeedge-evaluator-child/operation-catalog.v1.json"
assets="$root/deployments/codeedge-evaluator-child/contract-assets.v1.json"
profile="$root/deployments/codeedge-evaluator-child/execution-profile.v1.json"
contract_root="$root/deployments/codeedge-evaluator-child"
output="$contract_root/operation-catalog.lock.json"

for file in \
  "$catalog" \
  "$assets" \
  "$profile" \
  "$contract_root/contracts/harbor-pass-at-four.v0.18.json" \
  "$contract_root/schemas/harbor-run-bundle.v0.18.json"; do
  if [[ ! -f "$file" || -L "$file" ]]; then
    die "required regular deployment file is unavailable: $file"
  fi
done
if [[ -e "$output" || -L "$output" ]]; then
  die "refusing to replace existing lock: $output"
fi

exec env GOFLAGS= go run -mod=readonly ./tools/codeedge-evaluator-lock-build \
  --source-root "$root" \
  --catalog "$catalog" \
  --asset-manifest "$assets" \
  --execution-profile "$profile" \
  --contract-root "$contract_root" \
  --output "$output" \
  --build-version "$HARBOR_FACTORY_BUILD_VERSION" \
  --lock-version "$HARBOR_FACTORY_CODEEDGE_EVALUATOR_LOCK_VERSION" \
  --git-executable "$git_executable" \
  --harbor-launcher "$harbor_launcher" \
  --python-interpreter "$python_interpreter" \
  --python-source-tree "$python_source_tree" \
  --docker-cli "$docker_executable"
