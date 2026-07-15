#!/usr/bin/env bash
set -euo pipefail

# Build a locally packageable Harbor Factory binary whose runtime identity is
# exactly the identity in deployments/codeedge-phase1/operation-catalog.lock.json.
# This script never reads or writes model endpoints, credentials, or env files.
#
# The output is published only after every payload and checksum has been built
# in a private sibling directory.  An existing output path (including a
# symlink) is always rejected instead of being replaced.

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

root="$(CDPATH= cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
requested_output="${1:-"$root/dist/codeedge-production"}"
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
# symlink is rejected too.  Resolve the parent once for all later operations;
# staging there guarantees that the final rename cannot cross filesystems.
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

# A generated package inside the source tree must itself be ignored.  This
# keeps the clean-source proof meaningful while preserving the documented
# default of ./dist/codeedge-production.
case "$output" in
  "$root"/*)
    if ! git -C "$root" check-ignore -q -- "$output"; then
      die "an output path inside the source tree must be Git-ignored"
    fi
    ;;
esac

catalog="$root/deployments/codeedge-phase1/operation-catalog.v1.json"
lock="$root/deployments/codeedge-phase1/operation-catalog.lock.json"
excluded_lock="deployments/codeedge-phase1/operation-catalog.lock.json"

source_commit="$(git -C "$root" rev-parse HEAD)"
source_epoch="$(git -C "$root" show -s --format=%ct "$source_commit")"
if [[ ! "$source_epoch" =~ ^[0-9]+$ ]]; then
  die "the source commit has no valid reproducible timestamp"
fi
export SOURCE_DATE_EPOCH="$source_epoch"

# The source manifest is a SHA-256 over the canonical Git tree listing with the
# self-referential lock omitted. The lock itself carries this value, so a later
# reviewed release can update the lock without creating a hash cycle.
source_manifest="sha256:$(git -C "$root" ls-tree -r --full-tree "$source_commit" | LC_ALL=C awk -F '\t' -v excluded="$excluded_lock" '$2 != excluded { print $0 }' | sha256sum | awk '{print $1}')"
# The lock's commit is reviewed provenance, not an equality constraint on the
# final packaging commit. The content manifest above is the binding source
# proof; excluding the lock prevents a self-referential commit/hash cycle.
ldflags="$(cd "$root" && env GOFLAGS= go run -mod=readonly ./tools/codeedge-production-build --catalog "$catalog" --lock "$lock" --source-manifest "$source_manifest")"

workdir="$(mktemp -d "$output_parent/.codeedge-production.XXXXXX")"
package="$workdir/package"
cleanup() {
  rm -rf -- "$workdir"
}
trap cleanup EXIT

mkdir -p "$package/deployments/codeedge-phase1"
(cd "$root" && env GOFLAGS= go build -mod=readonly -trimpath -buildvcs=false -ldflags "$ldflags -buildid=" -o "$package/harbor-factory" .)
chmod 0755 "$package" "$package/deployments" "$package/deployments/codeedge-phase1" "$package/harbor-factory"
install -m 0644 "$catalog" "$package/deployments/codeedge-phase1/operation-catalog.v1.json"
install -m 0644 "$lock" "$package/deployments/codeedge-phase1/operation-catalog.lock.json"
install -m 0644 "$root/deployments/codeedge-phase1/README.md" "$package/deployments/codeedge-phase1/README.md"

archive_name="harbor-factory-codeedge-production.tar.gz"
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
    harbor-factory \
    deployments | gzip -n -9 > "$archive_name"

  # SHA256SUMS deliberately excludes itself.  It covers every distributable
  # payload and the compressed archive, so the archive can be validated before
  # extraction and the colocated files can be validated afterwards.
  sha256sum \
    deployments/codeedge-phase1/README.md \
    deployments/codeedge-phase1/operation-catalog.lock.json \
    deployments/codeedge-phase1/operation-catalog.v1.json \
    harbor-factory \
    "$archive_name" > SHA256SUMS
)

# No source change, including an ignored output staging directory, may mask a
# change to the reviewed source while Go is compiling.
require_clean_source
if [[ -e "$output" || -L "$output" ]]; then
  die "refusing to replace output target created during packaging: $output"
fi

# GNU mv's no-clobber mode preserves an output that appears after the check
# above.  The remaining source directory proves that no publication occurred.
if ! mv -T -n -- "$package" "$output" || [[ -e "$package" || -L "$package" ]]; then
  die "output target appeared during atomic publication: $output"
fi
if [[ -L "$output" || ! -d "$output" ]]; then
  die "atomic publication did not create a regular output directory: $output"
fi

printf 'built %s\n' "$output/harbor-factory"
printf 'packaged %s\n' "$output/$archive_name"
