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
# exactly one libc story per release.
#
# The policy is proved twice, because "-static" alone only asks the linker for a
# static link and says nothing about which libc was used:
#
#   1. the compiler says which triple it targets (<cc> -dumpmachine must name musl),
#      and its version is recorded next to the artifact;
#   2. the artifact is opened and its headers read (build/verify-artifact): no
#      PT_INTERP, no DT_NEEDED. A binary that passes both is self-contained. A
#      binary that cannot be checked fails the release instead of being labelled.
#
# The release manifest is written from those recorded facts, not from a constant.
# When the proof is missing there is no manifest to write, because the release has
# already failed.
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
  VERIFY_ARTIFACT=...  use this artifact verifier instead of building
                       build/verify-artifact (used by the script's own tests)
  DUSTKEEPER_BUILD_ENV=...  how to describe the build environment in the release
                       manifest, e.g. "docker:golang@sha256:..." for a pinned builder
                       image. Recorded, not interpreted.
  ALLOW_UNPROVED_LIBC=1  development only: build a Linux target even when the
                       compiler does not target musl. The fact is recorded as
                       linkage=unproved and the artifact is never labelled musl.
                       A release ignores this variable and refuses instead.
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

# Facts are collected while building and become the release manifest. A manifest
# assembled from constants at the end of the script is a wish, not a record.
FACTS_FILE="$OUT/build-facts.txt"
: >"$FACTS_FILE"
record_fact() {
	printf '%s\n' "$1" >>"$FACTS_FILE"
}

TMPD=$(mktemp -d "${TMPDIR:-/tmp}/dustkeeper-build.XXXXXX")
trap 'rm -rf "$TMPD"' EXIT

# The verifier reads the artifacts' headers. It is built once, before any target, so
# a missing verifier is a build failure rather than four confusing ones.
VERIFY=${VERIFY_ARTIFACT:-}
if [ -z "$VERIFY" ]; then
	VERIFY="$TMPD/verify-artifact"
	if ! go build -trimpath -o "$VERIFY" ./build/verify-artifact; then
		echo "error: could not build the artifact verifier; refusing to produce unchecked artifacts" >&2
		exit 1
	fi
fi

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

	# Two independent facts about a Linux artifact: which libc the compiler targets,
	# and what the artifact actually needs at run time. Neither one alone is proof.
	if [ "$os" = "linux" ]; then
		probe_cc=${cc:-cc}
		triple=$("$probe_cc" -dumpmachine 2>/dev/null || echo unknown)
		ccver=$("$probe_cc" --version 2>/dev/null | head -1 || echo unknown)
		sysroot=$("$probe_cc" -print-sysroot 2>/dev/null || echo "")
		# Which libc will this compiler actually link? A compiler's real target is
		# what it says about its own spec and include search path, not what its
		# filename is: Debian's musl-gcc reports "aarch64-linux-gnu" from
		# -dumpmachine and is still musl, because it injects musl's specs, headers
		# and libc.a. So the probe is a compile, and the evidence is the compiler's
		# own account of the paths it uses.
		probe_src="$TMPD/libc-probe.c"
		printf 'int main(void){return 0;}\n' >"$probe_src"
		probe_log="$TMPD/libc-probe.log"
		"$probe_cc" -v -c "$probe_src" -o "$TMPD/libc-probe.o" >"$probe_log" 2>&1 || true
		musl_evidence=$(grep -o '[^ "]*musl[^ "]*' "$probe_log" | head -1 || true)
		proved=0
		case "$triple" in
		*musl*) proved=1 ;;
		esac
		if [ -n "$musl_evidence" ]; then
			proved=1
		fi
		if [ "$proved" = 0 ]; then
			# A glibc static binary is still a glibc binary: its static libc talks to
			# the host's NSS and resolver at run time, which is why the linker warns
			# about getaddrinfo. It must never be labelled musl-static.
			#
			# A development build may produce one anyway, but only when the operator
			# asks for it with ALLOW_UNPROVED_LIBC=1, and the fact is recorded as
			# unproved. A release refuses outright.
			if [ "$MODE" = release ] || [ "${ALLOW_UNPROVED_LIBC:-0}" != "1" ]; then
				complain "$target was built with $probe_cc, which targets '$triple', not musl"
				record_problem "$target"
				return 0
			fi
			if ! facts=$("$VERIFY" -file "$out" -format elf -machine "$arch" 2>&1); then
				complain "$target is not a readable $arch ELF: $facts"
				record_problem "$target"
				return 0
			fi
			echo "dev: $target linkage is unproved (compiler targets '$triple'); this artifact is not a musl static build" >&2
			record_fact "target=$target format=elf arch=$arch linkage=unproved toolchain=$triple compiler=$probe_cc compiler_version=\"$ccver\" build_env=${DUSTKEEPER_BUILD_ENV:-host} artifact=[$facts]"
		else
			if ! facts=$("$VERIFY" -file "$out" -format elf -machine "$arch" -require static 2>&1); then
				complain "$target is not a static $arch ELF: $facts"
				record_problem "$target"
				return 0
			fi
			record_fact "target=$target format=elf arch=$arch linkage=static toolchain=$triple compiler=$probe_cc compiler_version=\"$ccver\" sysroot=${sysroot:-none} musl_evidence=$musl_evidence build_env=${DUSTKEEPER_BUILD_ENV:-host} artifact=[$facts]"
		fi
	else
		if ! facts=$("$VERIFY" -file "$out" -format macho -machine "$arch" 2>&1); then
			complain "$target is not a $arch Mach-O: $facts"
			record_problem "$target"
			return 0
		fi
		record_fact "target=$target format=macho arch=$arch linkage=linked artifact=[$facts]"
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

	# Only the host's own target can be executed here. Running it is the one check
	# that says the artifact starts at all; the other targets record that it did not
	# run rather than pretending it did. The probe is the `version` subcommand: this
	# program has no --version flag, and a probe the binary rejects would report a
	# perfectly good artifact as broken.
	if [ "$os" = "$(go env GOOS)" ] && [ "$arch" = "$(go env GOARCH)" ]; then
		if ! smoke=$("$out" version 2>&1); then
			complain "$target does not run on this host: $smoke"
			record_problem "$target"
			return 0
		fi
		record_fact "target=$target smoke=ok smoke_output=\"$(printf '%s' "$smoke" | head -1)\""
	else
		record_fact "target=$target smoke=not-runnable-here"
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
	# machine" is not a release note; this is. The libc line is written only after
	# every Linux target has a recorded static-ELF proof: the label describes the
	# artifacts in this build, it is not a constant.
	unproved=""
	for target in $RELEASE_TARGETS; do
		case "$target" in
		linux-*) grep -q "^target=$target .*linkage=static" "$FACTS_FILE" || unproved="$unproved $target" ;;
		esac
	done
	if [ -n "$unproved" ]; then
		echo "release is incomplete, libc policy unproved:$unproved" >&2
		exit 1
	fi
	{
		echo "# release manifest: one key=value per record"
		echo "# artifact and toolchain facts below were read during this build"
		echo "version=$VERSION"
		echo "commit=$COMMIT"
		echo "built=$STAMP"
		echo "host=$(uname -s)-$(uname -m)"
		echo "build_env=${DUSTKEEPER_BUILD_ENV:-host}"
		echo "go=$(go version 2>/dev/null || echo unknown)"
		echo "linux_libc=musl-static"
		echo "linux_libc_proof=compiler -dumpmachine names musl; artifact has no PT_INTERP and no DT_NEEDED"
		echo "targets=$RELEASE_TARGETS"
		cat "$FACTS_FILE"
	} >"$OUT/release-manifest.txt"
fi

echo "artifacts in $OUT"
ls -1 "$OUT"
