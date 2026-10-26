#!/bin/sh
# Build dustkeeper.
#
# The binary links SQLite through cgo, so a cross build needs a C compiler for
# the target platform. Set CC_<os>_<arch> to use one; without it only the native
# target is built. Nothing here publishes or installs anything.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-1.0.0-phase1}
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
STAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUT="$ROOT/dist"

mkdir -p "$OUT"

# sqlite_omit_load_extension is required by section 3.1: the released binary
# must not expose SQL extension loading at all. It is a cgo build tag, so it has
# to be set here rather than in Go source.
TAGS="sqlite_omit_load_extension"

LDFLAGS="-X main.commit=$COMMIT -X main.buildTags=$TAGS -X main.buildStamp=$STAMP"
if [ "${DEBUG:-0}" = "1" ]; then
	LDFLAGS="$LDFLAGS"
else
	LDFLAGS="$LDFLAGS -s -w"
fi

build_one() {
	os=$1
	arch=$2
	cc_var="CC_${os}_${arch}"
	eval "cc=\${$cc_var:-}"

	out="$OUT/dustkeeper-$VERSION-$os-$arch"
	if [ -z "$cc" ] && [ "$os-$arch" != "$(go env GOOS)-$(go env GOARCH)" ]; then
		echo "skip $os/$arch: set $cc_var to a C compiler for that target"
		return 0
	fi

	echo "building $os/$arch"
	env \
		GOOS="$os" GOARCH="$arch" CGO_ENABLED=1 \
		${cc:+CC="$cc"} \
		go build -trimpath -buildvcs=false -tags "$TAGS" -ldflags "$LDFLAGS" \
		-o "$out" ./cmd/dustkeeper

	if [ "$os" = "darwin" ]; then
		codesign --force --sign - "$out" >/dev/null 2>&1 || true
	fi
}

build_one darwin arm64
build_one darwin amd64
build_one linux amd64
build_one linux arm64

echo "version=$VERSION commit=$COMMIT built=$STAMP"

# The checksums cover exactly the artifacts in dist, so a downloaded file can be
# verified without trusting the download page.
( cd "$OUT" && ls -1 dustkeeper-* 2>/dev/null | grep -v '\.sha256$' | while read -r f; do
	shasum -a 256 "$f"
done > SHA256SUMS ) || true

echo "artifacts in $OUT"
ls -1 "$OUT"
