#!/usr/bin/env bash
set -euo pipefail

# Build the local, immutable Harbor Flow production package. The package binds
# the Standard authoring closed template. It intentionally reads neither
# provider endpoints nor credentials.
#
# Every catalog/lock pair is copied into a private staging directory before it
# is verified and linked. The binary and its colocated deployment payloads
# therefore come from the same immutable input snapshot, even if a caller
# changes a source file while packaging is in progress.

umask 022
export LC_ALL=C
export TZ=UTC

die() {
  echo "build-codeedge-production: $*" >&2
  exit 1
}

require_clean_source() {
  if [[ -n "$(git -C "$root" status --porcelain=v1 --untracked-files=all)" ]]; then
    die "a clean committed source tree is required"
  fi
}

require_regular_file() {
  local label="$1"
  local path="$2"
  if [[ ! -f "$path" || -L "$path" ]]; then
    die "$label must be a regular non-symlink file: $path"
  fi
}

copy_deployment_tree() {
  local label="$1"
  local source="$2"
  local destination="$3"
  local exclude_candidates="$4"
  local entry relative target

  if [[ ! -d "$source" || -L "$source" ]]; then
    die "$label deployment directory must be a non-symlink directory: $source"
  fi
  mkdir -p -- "$destination"
  while IFS= read -r -d '' entry; do
    relative="${entry#"$source"/}"
    if [[ "$exclude_candidates" == "1" && ( "$relative" == "candidates" || "$relative" == candidates/* ) ]]; then
      continue
    fi
    target="$destination/$relative"
    if [[ -L "$entry" ]]; then
      die "$label deployment contains a symlink: $relative"
    fi
    if [[ -d "$entry" ]]; then
      mkdir -p -- "$target"
    elif [[ -f "$entry" ]]; then
      mkdir -p -- "$(dirname -- "$target")"
      install -m 0644 "$entry" "$target"
    else
      die "$label deployment contains a non-regular file: $relative"
    fi
  done < <(find -P "$source" -mindepth 1 -print0)
}

reject_nonproduction_material() {
  local root="$1"
  local unexpected
  unexpected="$(find -P "$root" \( -path '*/candidates' -o -path '*/candidates/*' -o -iname '*discovery*' \) -print -quit)"
  if [[ -n "$unexpected" ]]; then
    die "candidate or discovery material is not a production payload: ${unexpected#"$root"/}"
  fi
}

root="$(CDPATH= cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
requested_output="${1:-"$root/dist/harbor-flow-production"}"
if [[ "$requested_output" != /* ]]; then
  requested_output="$PWD/$requested_output"
fi
while [[ "$requested_output" != "/" && "$requested_output" == */ ]]; do
  requested_output="${requested_output%/}"
done
if [[ "$requested_output" == "/" ]]; then
  die "the output path must name a new directory"
fi

output_name="$(basename -- "$requested_output")"
if [[ -z "$output_name" || "$output_name" == "." || "$output_name" == ".." ]]; then
  die "the output path must name a new directory"
fi
output_parent="$(dirname -- "$requested_output")"

# Test the requested spelling before resolving its parent so that a dangling
# symlink is rejected too. Resolve the parent once, which keeps final rename
# within one filesystem.
if [[ -e "$requested_output" || -L "$requested_output" ]]; then
  die "refusing to replace existing or symlink output target: $requested_output"
fi

require_clean_source

mkdir -p -- "$output_parent"
output_parent="$(CDPATH= cd -P -- "$output_parent" && pwd)"
output="$output_parent/$output_name"
if [[ -e "$output" || -L "$output" ]]; then
  die "refusing to replace existing or symlink output target: $output"
fi

# A package written beneath the source tree must be ignored. This preserves
# the clean-source proof while retaining the documented default output path.
case "$output" in
  "$root"/*)
    if ! git -C "$root" check-ignore -q -- "$output"; then
      die "an output path inside the source tree must be Git-ignored"
    fi
    ;;
esac

standard_catalog="$root/deployments/standard-authoring/operation-catalog.v1.json"
standard_lock="$root/deployments/standard-authoring/operation-catalog.lock.json"
production_package_readme="$root/docs/PRODUCTION_PACKAGE.md"

for entry in \
  "Standard authoring catalog:$standard_catalog" \
  "Standard authoring lock:$standard_lock" \
  "Production package README:$production_package_readme"; do
  require_regular_file "${entry%%:*}" "${entry#*:}"
done

source_commit="$(git -C "$root" rev-parse HEAD)"
source_epoch="$(git -C "$root" show -s --format=%ct "$source_commit")"
if [[ ! "$source_epoch" =~ ^[0-9]+$ ]]; then
  die "the source commit has no valid reproducible timestamp"
fi
export SOURCE_DATE_EPOCH="$source_epoch"

# The generated lock is omitted from the tree manifest. A lock carries this
# digest itself, so including it would create a hash cycle.
source_manifest="sha256:$(git -C "$root" ls-tree -r --full-tree "$source_commit" | LC_ALL=C awk -F '\t' '$2 != "deployments/standard-authoring/operation-catalog.lock.json" { print $0 }' | sha256sum | awk '{print $1}')"

workdir="$(mktemp -d "$output_parent/.harbor-flow-production.XXXXXX")"
inputs="$workdir/inputs"
package="$workdir/package"
cleanup() {
  rm -rf -- "$workdir"
}
trap cleanup EXIT

copy_deployment_tree "Standard authoring" "$root/deployments/standard-authoring" "$inputs/deployments/standard-authoring" 0
reject_nonproduction_material "$inputs/deployments"

ldflags="$(cd "$root" && env GOFLAGS= go run -mod=readonly ./tools/harbor-flow-production-build \
  --standard-authoring-catalog "$inputs/deployments/standard-authoring/operation-catalog.v1.json" \
  --standard-authoring-lock "$inputs/deployments/standard-authoring/operation-catalog.lock.json" \
  --source-manifest "$source_manifest")"

mkdir -p "$package/deployments"
install -m 0644 "$production_package_readme" "$package/README.md"
copy_deployment_tree "staged Standard authoring" "$inputs/deployments/standard-authoring" "$package/deployments/standard-authoring" 0
reject_nonproduction_material "$package/deployments"

(cd "$root" && env GOFLAGS= go build -mod=readonly -trimpath -buildvcs=false -ldflags "$ldflags -buildid=" -o "$package/harbor-factory" .)
find -P "$package/deployments" -type d -exec chmod 0755 {} +
find -P "$package/deployments" -type f -exec chmod 0644 {} +
chmod 0755 "$package" "$package/deployments" "$package/harbor-factory"

archive_name="harbor-factory-harbor-flow-production.tar.gz"
(
  cd "$package"
  tar \
    --sort=name \
    --mtime="@$SOURCE_DATE_EPOCH" \
    --owner=0 \
    --group=0 \
    --numeric-owner \
    --format=posix \
    --pax-option=delete=atime,delete=ctime \
    -cf - \
    README.md \
    harbor-factory \
    deployments | gzip -n -9 > "$archive_name"

  mapfile -d '' package_payloads < <(find -P deployments -type f -print0 | LC_ALL=C sort -z)
  package_payloads+=(README.md harbor-factory "$archive_name")
  sha256sum "${package_payloads[@]}" > SHA256SUMS
)

# No source change may appear while Go is compiling. The staged inputs above
# separately prevent a lock/catalog change from drifting between verification
# and publication.
require_clean_source
if [[ -e "$output" || -L "$output" ]]; then
  die "refusing to replace output target created during packaging: $output"
fi

if ! mv -T -n -- "$package" "$output" || [[ -e "$package" || -L "$package" ]]; then
  die "output target appeared during atomic publication: $output"
fi
if [[ -L "$output" || ! -d "$output" ]]; then
  die "atomic publication did not create a regular output directory: $output"
fi

printf 'built %s\n' "$output/harbor-factory"
printf 'packaged %s\n' "$output/$archive_name"
