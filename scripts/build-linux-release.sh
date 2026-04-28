#!/usr/bin/env sh
set -eu

VERSION="${VERSION:-dev}"
TARGET_OS="${TARGET_OS:-linux}"
TARGET_ARCH="${TARGET_ARCH:-amd64}"
DIST_DIR="${DIST_DIR:-dist}"
MAIN_PACKAGE="${MAIN_PACKAGE:-./cmd/p2r}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || printf unknown)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
ROOT_DIR="$(pwd)"
GOCACHE="${GOCACHE:-${ROOT_DIR}/.go-cache}"
GOMODCACHE="${GOMODCACHE:-${ROOT_DIR}/.gomodcache}"
export GOCACHE GOMODCACHE

PACKAGE="p2r_${VERSION}_${TARGET_OS}_${TARGET_ARCH}"
DIST_ABS="$(mkdir -p "$DIST_DIR" && cd "$DIST_DIR" && pwd)"
WORK_DIR="${DIST_ABS}/${PACKAGE}"
ARCHIVE="${DIST_ABS}/${PACKAGE}.tar.gz"
CHECKSUM="${ARCHIVE}.sha256"

case "$WORK_DIR" in
	"$DIST_ABS"/*) rm -rf "$WORK_DIR" ;;
	*) echo "refusing to clean unsafe release path: $WORK_DIR" >&2; exit 1 ;;
esac
rm -f "$ARCHIVE" "$CHECKSUM"

mkdir -p "$WORK_DIR"

LDFLAGS="-s -w"
LDFLAGS="$LDFLAGS -X github.com/xuanli520/p2r_tui/cmd.version=$VERSION"
LDFLAGS="$LDFLAGS -X github.com/xuanli520/p2r_tui/cmd.commit=$COMMIT"
LDFLAGS="$LDFLAGS -X github.com/xuanli520/p2r_tui/cmd.buildDate=$BUILD_DATE"

CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" go build -trimpath -ldflags "$LDFLAGS" -o "$WORK_DIR/p2r" "$MAIN_PACKAGE"

cp README.md "$WORK_DIR/README.md"
cp docs/linux-release.md "$WORK_DIR/INSTALL.md"
cp .p2r.yaml "$WORK_DIR/p2r.example.yaml"

(cd "$DIST_ABS" && tar -czf "${PACKAGE}.tar.gz" "$PACKAGE")

if command -v sha256sum >/dev/null 2>&1; then
	(cd "$DIST_ABS" && sha256sum "${PACKAGE}.tar.gz" > "${PACKAGE}.tar.gz.sha256")
else
	echo "sha256sum not found; checksum was not written" >&2
fi

printf '%s\n' "$ARCHIVE"
