#!/bin/sh
# Build dustkeeper.
#
# Two modes, because "quietly skipped" and "shipped anyway" must not be the same
# outcome:
#
#   build/build.sh             development. A target whose cross C compiler is
#                              missing is skipped, with a note.
#   build/build.sh --release   release. Every target in RELEASE_TARGETS must be
#                              produced, signed where the platform requires it,
#                              listed in SHA256SUMS and re-verified. Anything
#                              short of that exits non-zero.
#
# Why cgo at all: SQLite is linked through cgo, so a cross build needs a C
# compiler for the *target* platform. There is no pure-Go fallback, and pretending
# otherwise would ship a binary that cannot open its own database.
#
# Linux libc policy (fixed, not "whatever the build host happened to have"):
# Linux artifacts are statically linked against musl. Point CC_linux_amd64 /
# CC_linux_arm64 at a musl cross compiler (x86_64-linux-musl-gcc,
# aarch64-linux-musl-gcc). A glibc-linked build is not produced, so there is
# exactly one libc story per release. The policy is enforced by passing
# "-linkmode external -extldflags -static".
#
# Windows is not a target: the tree relies on unix syscalls (flock, renameatx_np /
# renameat2, F_FULLFSYNC) that have no equivalent in this design. Saying so here
# is better than a target list that quietly never builds. Only darwin/arm64 is
# exercised by the test suite on the maintainer's machine; the other declared
# targets are built, not verified, and must be described as such in the docs.
#
# Nothing here publishes or installs anything.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

usage() {
	cat <<'EOF'
usage: build/build.sh [--release|--dev]

  --dev       (default) build what this host can build, skip the rest with a note
  --release   require every declared target, its signature and its checksum

environment:
  VERSION=...          version stamped into the binary (default 1.0.0-phase1)
  CC_<os>_<arch>=...   C compiler for a cross target, e.g.
                       CC_linux_amd64=x86_64-linux-musl-gcc
  DEBUG=1              keep symbols (no -s -w)
  RELEASE=1            same as --release
EOF
}

MODE=dev
for arg in "$@"; do
	case "$arg" in
	--release) MODE=release ;;
	--dev) MODE=dev ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown argument: $arg (try --help)" >&2
		exit 2
		;;
	esac
done
if [ "${RELEASE:-0}" = "1" ]; then
	MODE=release
fi

VERSION=${VERSION:-1.0.0-phase1}
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
STAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUT="$ROOT/dist"

# The contract with the release: a target named here that produces no artifact has
# not been "skipped", it has failed.
RELEASE_TARGETS="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"

mkdir -p "$OUT"

# sqlite_omit_load_extension is required by section 3.1: the released binary must
# not expose SQL extension loading at all. It is a cgo build tag, so it has to be
# set here rather than in Go source.
TAGS="sqlite_omit_load_extension"

LDFLAGS="-X main.commit=$COMMIT -X main.buildTags=$TAGS -X main.buildStamp=$STAMP"
if [ "${DEBUG:-0}" != "1" ]; then
	LDFLAGS="$LDFLAGS -s -w"
fi

BUILT=""
PROBLEMS=""
FAILED=0

# record_problem remembers a target once, so the final report reads as a list.
record_problem() {
	case " $PROBLEMS " in
	*" $1 "*) return 0 ;;
	esac
	PROBLEMS="$PROBLEMS $1"
}

# complain reports a target-level problem. A release treats it as fatal, a
# development build as advisory; neither one swallows it.
complain() {
	echo "$MODE: $1" >&2
	if [ "$MODE" = release ]; then
		FAILED=1
	fi
}

build_one() {
	os=$1
	arch=$2
	target="$os-$arch"
	cc_var="CC_${os}_${arch}"
	eval "cc=\${$cc_var:-}"
	out="$OUT/dustkeeper-$VERSION-$target"

	if [ -z "$cc" ] && [ "$target" != "$(go env GOOS)-$(go env GOARCH)" ]; then
		complain "$target is not built: set $cc_var to a C compiler for that target"
		record_problem "$target"
		return 0
	fi

	echo "building $target"
	ldflags="$LDFLAGS"
	if [ "$os" = "linux" ]; then
		# Fixed musl static policy; see the header.
		ldflags="$ldflags -linkmode external -extldflags \"-static\""
	fi

	if [ -n "$cc" ]; then
		CC="$cc"
		export CC
	fi

	if ! env GOOS="$os" GOARCH="$arch" CGO_ENABLED=1 \
		go build -trimpath -buildvcs=false -tags "$TAGS" -ldflags "$ldflags" \
		-o "$out" ./cmd/dustkeeper; then
		complain "$target failed to build"
		record_problem "$target"
		unset CC
		return 0
	fi
	unset CC

	if [ ! -s "$out" ]; then
		complain "$target produced an empty artifact"
		record_problem "$target"
		return 0
	fi

	if [ "$os" = "darwin" ]; then
		if ! codesign --force --sign - "$out" >/dev/null 2>&1; then
			complain "$target could not be signed"
			record_problem "$target"
			return 0
		fi
		if ! codesign --verify --strict "$out" >/dev/null 2>&1; then
			complain "$target carries a signature that does not verify"
			record_problem "$target"
			return 0
		fi
	fi

	BUILT="$BUILT $target"
}

for target in $RELEASE_TARGETS; do
	build_one "${target%%-*}" "${target##*-}"
done

echo "version=$VERSION commit=$COMMIT built=$STAMP mode=$MODE"

# Every declared target has to be in BUILT, not merely "attempted": a build step
# that returns 0 without leaving an artifact is still a missing target.
for target in $RELEASE_TARGETS; do
	case " $BUILT " in
	*" $target "*) ;;
	*) record_problem "$target" ;;
	esac
done

if [ "$FAILED" -eq 1 ]; then
	echo "release is incomplete, missing:$PROBLEMS" >&2
	exit 1
fi

# A checksum file is worthless if it can be empty, partial or stale, so it is
# generated from exactly the artifacts built here and then verified again.
SHA=""
if command -v shasum >/dev/null 2>&1; then
	SHA="shasum -a 256"
elif command -v sha256sum >/dev/null 2>&1; then
	SHA="sha256sum"
fi

FILES=""
for target in $BUILT; do
	FILES="$FILES dustkeeper-$VERSION-$target"
done

if [ -z "$SHA" ]; then
	echo "error: no SHA-256 tool found; refusing to produce unverified artifacts" >&2
	exit 1
fi
if [ -z "$FILES" ]; then
	echo "error: nothing was built, so there is nothing to checksum" >&2
	exit 1
fi

# The old "ls dustkeeper-*" glob is gone on purpose: a leftover artifact from an
# earlier run must not slip into the checksum file and get published as part of
# this build.
rm -f "$OUT/SHA256SUMS"
# shellcheck disable=SC2086
if ! (cd "$OUT" && $SHA $FILES >SHA256SUMS); then
	echo "error: could not write $OUT/SHA256SUMS" >&2
	exit 1
fi
# shellcheck disable=SC2086
if ! (cd "$OUT" && $SHA -c SHA256SUMS >/dev/null); then
	echo "error: $OUT/SHA256SUMS does not verify" >&2
	exit 1
fi

want=$(printf '%s\n' $FILES | wc -l | tr -d ' ')
got=$(wc -l <"$OUT/SHA256SUMS" | tr -d ' ')
if [ "$want" != "$got" ]; then
	echo "error: SHA256SUMS covers $got of $want artifacts" >&2
	exit 1
fi

if [ "$MODE" = release ]; then
	uncovered=""
	for target in $RELEASE_TARGETS; do
		grep -q "  dustkeeper-$VERSION-$target\$" "$OUT/SHA256SUMS" || uncovered="$uncovered $target"
	done
	if [ -n "$uncovered" ]; then
		echo "release is incomplete, unchecksummed:$uncovered" >&2
		exit 1
	fi

	# Record what was produced, under which policy, on which host. "It built on my
	# machine" is not a release note; this is.
	{
		echo "version=$VERSION"
		echo "commit=$COMMIT"
		echo "built=$STAMP"
		echo "host=$(uname -s)-$(uname -m)"
		echo "go=$(go version 2>/dev/null || echo unknown)"
		echo "linux_libc=musl-static"
		echo "targets=$RELEASE_TARGETS"
	} >"$OUT/release-manifest.txt"
fi

echo "artifacts in $OUT"
ls -1 "$OUT"
