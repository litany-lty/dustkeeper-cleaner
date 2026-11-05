#!/bin/sh
# Build dustkeeper for the machine it is running on.
#
# One target per run, natively compiled. There is no cross-target list any more:
# a release is the artifact this host produced, and a host that is not one of the
# two release targets produces a development build that is explicitly not
# labelled supported.
#
#   build/build.sh              development build of the host target
#   build/build.sh --release    the same build, with every check mandatory
#
# Two supported release targets, and nothing else is called supported:
#
#   darwin-arm64   native arm64 compile, ad-hoc signature, architecture check,
#                  `version` and a quarantine/restore smoke in a temporary tree
#   linux-amd64    musl static build, produced only through
#                  build/build-linux-release.sh (pinned builder image); the same
#                  artifact and smoke checks apply
#
# Why cgo at all: SQLite is linked through cgo, so there is no pure-Go fallback;
# pretending otherwise would ship a binary that cannot open its own database. The
# compiler comes from the environment (`CC`), which is how the container entry
# point supplies musl-gcc. This script never probes for a cross compiler and never
# substitutes one: the artifact's architecture is read from the artifact.
#
# Linux libc policy (fixed, not "whatever the build host happened to have"):
# a Linux artifact must be statically linked against musl, and the claim is proved
# twice, because "-static" alone only asks the linker for a static link:
#
#   1. the link inputs the compiler really passes to the linker name musl -- the
#      startfile (crt1.o/Scrt1.o) and the -L it hands to collect2, read back from a
#      static link of a probe. Not a directory that happens to appear in the
#      include search list: CPATH can add any directory there, so an empty one
#      called *musl* would otherwise prove a glibc compiler. Not
#      -print-file-name=libc.a either: Debian's musl-gcc answers that with glibc's
#      path while linking musl's archive. Not the banner or -print-sysroot: they
#      can say anything. A compiler that targets musl outright (triple names musl)
#      is its own evidence, because that is the driver's answer about itself;
#   2. the artifact is opened and its headers read (build/verify-artifact): no
#      PT_INTERP, no DT_NEEDED. A binary that cannot be checked fails the release
#      instead of being labelled.
#
# The release manifest is written from those recorded facts, not from constants.
# When a proof is missing there is no manifest, because the release already failed.
#
# Windows is not a target: the tree relies on unix syscalls (flock, renameatx_np /
# renameat2, F_FULLFSYNC) that have no equivalent in this design.
#
# Nothing here publishes or installs anything.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

usage() {
	cat <<'EOF'
usage: build/build.sh [--release|--dev]

  --dev       (default) build the host target and report every problem
  --release   the same build, where any failed check exits non-zero

environment:
  VERSION=...          version stamped into the binary (default 1.0.0-phase1)
  CC=...               C compiler for this host (musl-gcc inside the Linux
                       container entry point). Never a cross compiler.
  VERIFY_ARTIFACT=...  use this artifact verifier instead of building
                       build/verify-artifact (used by the script's own tests)
  REVIEW_SMOKE_HELPER=... use this prebuilt review-smoke driver instead of
                       building build/review-smoke
  DUSTKEEPER_BUILD_ENV=...  how to describe the build environment in the release
                       manifest, e.g. "docker:golang@sha256:..." for a pinned
                       builder image. Recorded, not interpreted.
  DUSTKEEPER_MUSL_PACKAGES=...  the musl packages the Linux builder installed, e.g.
                       "musl=1.2.3-1,musl-dev=1.2.3-1". Recorded next to the libc
                       proof; not interpreted.
  ALLOW_UNPROVED_LIBC=1  development only: keep a Linux artifact whose libc could
                       not be proved. Recorded as linkage=unproved and never
                       labelled musl. A release ignores this variable.
  SKIP_SMOKE=1         development only: skip the run smoke. A release ignores it.
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

# The contract with the release: these two, built on their own machine. A host
# that is neither produces a development build, and the difference is recorded in
# the artifact name and in the facts file rather than left to the reader.
RELEASE_TARGETS="darwin-arm64 linux-amd64"

# The host is the machine the toolchain runs on, which GOHOSTOS/GOHOSTARCH
# report. GOOS/GOARCH are target settings: an exported GOOS or a CI image default
# can point the build at another system, and then "darwin-arm64" in the artifact
# name would be a claim about the host that the build never made. The two must
# agree or this script refuses to name anything.
HOST_OS=$(go env GOHOSTOS)
HOST_ARCH=$(go env GOHOSTARCH)
BUILD_OS=$(go env GOOS)
BUILD_ARCH=$(go env GOARCH)
if [ "$BUILD_OS-$BUILD_ARCH" != "$HOST_OS-$HOST_ARCH" ]; then
	echo "error: the toolchain targets $BUILD_OS-$BUILD_ARCH but runs on $HOST_OS-$HOST_ARCH" >&2
	echo "       unset GOOS/GOARCH: this script builds the host's artifact, and naming a" >&2
	echo "       cross-compiled binary after the host is what the release gate forbids" >&2
	exit 1
fi
TARGET="$HOST_OS-$HOST_ARCH"

mkdir -p "$OUT"

# sqlite_omit_load_extension is required by section 3.1: the released binary must
# not expose SQL extension loading at all. It is a cgo build tag, so it has to be
# set here rather than in Go source.
TAGS="sqlite_omit_load_extension"

LDFLAGS="-X main.commit=$COMMIT -X main.buildTags=$TAGS -X main.buildStamp=$STAMP"
if [ "${DEBUG:-0}" != "1" ]; then
	LDFLAGS="$LDFLAGS -s -w"
fi

# Facts are collected while building and become the release manifest. A manifest
# assembled from constants at the end of the script is a wish, not a record.
FACTS_FILE="$OUT/build-facts.txt"
: >"$FACTS_FILE"

# Same reason for the manifest: dist holds one release claim at a time, and the
# manifest names the artifact this run verified. Last run's manifest left sitting
# there while a new build is in progress is exactly how a build that failed halfway
# turns into something a person reads as a release. Only this file and this run's
# artifact are touched: other versions' binaries in dist are not this script's.
rm -f "$OUT/release-manifest.txt"
record_fact() {
	printf '%s\n' "$1" >>"$FACTS_FILE"
}

TMPD=$(mktemp -d "${TMPDIR:-/tmp}/dustkeeper-build.XXXXXX")
trap 'rm -rf "$TMPD"' EXIT

# fail_release is the single way out of the stages after the build. Every one of
# them exists to prove something about the artifact in dist, so a failure there
# means this run proved nothing: the binary it would have published, this run's
# checksum file and the manifest that would have cited them are all removed.
# Leaving them behind produces a dist directory that looks like a release and
# verifies as one, which is the confusion the checksum step exists to rule out.
fail_release() {
	echo "error: $1" >&2
	if [ -n "${BUILT:-}" ]; then
		rm -f "$BUILT"
	fi
	rm -f "$OUT/SHA256SUMS" "$OUT/release-manifest.txt"
	exit 1
}

# complain reports a problem. A release treats it as fatal, a development build as
# advisory; neither one swallows it.
FAILED=0
complain() {
	echo "$MODE: $1" >&2
	if [ "$MODE" = release ]; then
		FAILED=1
	fi
}

# fail_target reports a problem and removes the artifact the problem is about. A
# binary that failed a check must not stay in dist where a later step, or a person,
# could mistake it for something that passed: "no artifact" is the honest result.
fail_target() {
	complain "$1"
	rm -f "$out"
}

supported=no
for t in $RELEASE_TARGETS; do
	if [ "$t" = "$TARGET" ]; then supported=yes; fi
done

if [ "$MODE" = release ] && [ "$supported" != yes ]; then
	# The old gate failed because three of four targets were missing on a
	# developer's machine. This one is about the artifact that will exist: a host
	# that is not a release target cannot produce a release, and saying so is the
	# whole difference between "released" and "built here once".
	echo "error: $TARGET is not a release target" >&2
	echo "       release artifacts are built on: $RELEASE_TARGETS" >&2
	echo "       this machine produced nothing; use --dev for a local binary" >&2
	exit 1
fi
if [ "$supported" != yes ]; then
	echo "dev: $TARGET is not a release target; this build is not labelled supported" >&2
fi

# The verifier reads the artifacts' headers. It is built once, before the target,
# so a missing verifier is one clear failure rather than a confusing artifact.
VERIFY=${VERIFY_ARTIFACT:-}
if [ -z "$VERIFY" ]; then
	VERIFY="$TMPD/verify-artifact"
	# -buildvcs=false for the same reason the artifact build uses it: inside the
	# builder container the mounted checkout belongs to another uid, git refuses it
	# as "dubious ownership", and VCS stamping would turn that into a build failure
	# that has nothing to do with the verifier.
	if ! go build -trimpath -buildvcs=false -o "$VERIFY" ./build/verify-artifact; then
		echo "error: could not build the artifact verifier; refusing to produce unchecked artifacts" >&2
		exit 1
	fi
fi

# The smoke driver owns its loopback TLS fake models and invokes the artifact as
# a subprocess. Building it separately keeps test-only trust material and fake
# responses out of the production binary.
REVIEW_SMOKE=${REVIEW_SMOKE_HELPER:-}
if [ -z "$REVIEW_SMOKE" ]; then
	REVIEW_SMOKE="$TMPD/review-smoke"
	if ! go build -trimpath -buildvcs=false -o "$REVIEW_SMOKE" ./build/review-smoke; then
		echo "error: could not build the review smoke helper; refusing to produce unchecked artifacts" >&2
		exit 1
	fi
fi

# smoke_qr drives the real artifact through two isolated installations. A local
# TLS server first rejects the candidate and must leave its bytes in place; the
# second installation supplies explicit evidence plus an independent retained
# copy, receives two fake-model approvals, quarantines, lists and restores the
# object, then compares its bytes. The helper's CA, credentials, evidence, state
# and trees are private temporary files. No production bypass is involved.
smoke_qr() {
	art=$1
	work=$2
	"$REVIEW_SMOKE" --artifact "$art" --work "$work"
}

build_target() {
	out="$OUT/dustkeeper-$VERSION-$TARGET"

	# No GOOS/GOARCH override: the artifact is compiled for this machine by this
	# machine, and its architecture is then read back from the artifact below.
	if [ "$HOST_OS" = "linux" ]; then
		# Fixed musl static policy; see the header.
		ldflags="$LDFLAGS -linkmode external -extldflags \"-static\""
	else
		ldflags="$LDFLAGS"
	fi

	if ! env CGO_ENABLED=1 go build -trimpath -buildvcs=false -tags "$TAGS" -ldflags "$ldflags" \
		-o "$out" ./cmd/dustkeeper; then
		fail_target "$TARGET failed to build"
		return 0
	fi
	if [ ! -s "$out" ]; then
		fail_target "$TARGET produced an empty artifact"
		return 0
	fi

	if [ "$HOST_OS" = "linux" ]; then
		probe_cc=${CC:-cc}
		triple=$("$probe_cc" -dumpmachine 2>/dev/null || echo unknown)
		ccver=$("$probe_cc" --version 2>/dev/null | head -1 || echo unknown)
		sysroot=$("$probe_cc" -print-sysroot 2>/dev/null || echo "")
		# Which libc will this artifact link? The answer has to come from the link
		# itself, because every cheaper answer can be made to say "musl" while the
		# linker reads glibc:
		#
		#   * anything CPATH names is prepended to the include search list printed by
		#     -v, so an empty directory called musl-headers-placeholder shows up in
		#     front of glibc's headers;
		#   * -print-file-name=libc.a answers with glibc's archive even for Debian's
		#     musl-gcc, whose specs send the real -lc somewhere else;
		#   * the banner and -print-sysroot are free text.
		#
		# So the probe is a real static link, and the command the driver then runs is
		# read back from its own transcript: the startfile it passes and the -L it
		# hands to collect2 are the inputs ld will open. musl-gcc puts both in musl's
		# directory; a glibc compiler puts both in glibc's, whatever CPATH says. A
		# driver that targets musl outright (its triple names musl) is its own
		# evidence, because that is its answer about itself.
		probe_src="$TMPD/libc-probe.c"
		printf 'int main(void){return 0;}\n' >"$probe_src"
		probe_log="$TMPD/libc-probe.log"
		# A probe that cannot compile or link is a failure, not a missing proof: it
		# says this compiler cannot produce the artifact at all.
		if ! "$probe_cc" -static -v -o "$TMPD/libc-probe" "$probe_src" >"$probe_log" 2>&1; then
			fail_target "$TARGET cannot be linked: the static libc probe failed with $probe_cc ($(tail -1 "$probe_log" 2>/dev/null))"
			return 0
		fi
		include_list=$(awk '/#include <\.\.\.> search starts here:/{f=1;next} /End of search list\./{f=0} f' \
			"$probe_log" 2>/dev/null || true)
		libc_a=$("$probe_cc" -print-file-name=libc.a 2>/dev/null || echo libc.a)
		link_line=$(grep -m1 -e 'collect2' -e ' -lc' "$probe_log" 2>/dev/null || true)
		startfile=""
		musl_libdir=""
		for tok in $link_line; do
			case "$tok" in
			*crt1.o) startfile=$tok ;;
			esac
			case "$tok" in
			-L*musl*) musl_libdir=$tok ;;
			esac
		done
		# Both have to come from musl: a startfile alone can be passed by hand, and a
		# -L alone does not say what the linker finds in that directory.
		musl_evidence=""
		proved=0
		case "$triple" in
		*musl*)
			proved=1
			musl_evidence="triple:$triple"
			;;
		esac
		if [ "$proved" = 0 ] && [ -n "$musl_libdir" ]; then
			case "$startfile" in
			*musl*)
				proved=1
				musl_evidence=$startfile
				;;
			esac
		fi
		if [ "$proved" = 0 ]; then
			# A glibc static binary is still a glibc binary: it must never be
			# labelled musl-static, and a release refuses it outright.
			if [ "$MODE" = release ] || [ "${ALLOW_UNPROVED_LIBC:-0}" != "1" ]; then
				fail_target "$TARGET was linked by $probe_cc with startfile '${startfile:-none}' and lib search '${musl_libdir:-no musl -L}', not musl"
				return 0
			fi
			if ! facts=$("$VERIFY" -file "$out" -format elf -machine "$HOST_ARCH" 2>&1); then
				fail_target "$TARGET is not a readable $HOST_ARCH ELF: $facts"
				return 0
			fi
			echo "dev: $TARGET linkage is unproved (compiler targets '$triple', startfile '${startfile:-none}'); this artifact is not a musl static build" >&2
			record_fact "target=$TARGET format=elf arch=$HOST_ARCH linkage=unproved toolchain=$triple compiler=$probe_cc compiler_version=\"$ccver\" startfile=${startfile:-none} libc_a=$libc_a musl_pkgs=${DUSTKEEPER_MUSL_PACKAGES:-unknown} include_search=\"$(printf '%s' "$include_list" | tr '\n' ' ' | sed 's/  */ /g')\" build_env=${DUSTKEEPER_BUILD_ENV:-host} artifact=[$facts]"
		else
			if ! facts=$("$VERIFY" -file "$out" -format elf -machine "$HOST_ARCH" -require static 2>&1); then
				fail_target "$TARGET is not a static $HOST_ARCH ELF: $facts"
				return 0
			fi
			record_fact "target=$TARGET format=elf arch=$HOST_ARCH linkage=static toolchain=$triple compiler=$probe_cc compiler_version=\"$ccver\" sysroot=${sysroot:-none} startfile=${startfile:-none} musl_libdir=${musl_libdir:-none} libc_a=$libc_a musl_evidence=$musl_evidence musl_pkgs=${DUSTKEEPER_MUSL_PACKAGES:-unknown} build_env=${DUSTKEEPER_BUILD_ENV:-host} artifact=[$facts]"
		fi
	else
		if ! facts=$("$VERIFY" -file "$out" -format macho -machine "$HOST_ARCH" 2>&1); then
			fail_target "$TARGET is not a $HOST_ARCH Mach-O: $facts"
			return 0
		fi
		record_fact "target=$TARGET format=macho arch=$HOST_ARCH linkage=linked build_env=${DUSTKEEPER_BUILD_ENV:-host} artifact=[$facts]"
	fi

	if [ "$HOST_OS" = "darwin" ]; then
		if ! codesign --force --sign - "$out" >/dev/null 2>&1; then
			fail_target "$TARGET could not be signed"
			return 0
		fi
		if ! codesign --verify --strict "$out" >/dev/null 2>&1; then
			fail_target "$TARGET carries a signature that does not verify"
			return 0
		fi
	fi

	# Running the artifact is the one check that says it starts at all. The probe
	# is the `version` subcommand: this program has no --version flag, and a probe
	# the binary rejects would report a perfectly good artifact as broken.
	if [ "${SKIP_SMOKE:-0}" = "1" ] && [ "$MODE" = dev ]; then
		record_fact "target=$TARGET smoke=skipped-by-request"
		echo "dev: smoke skipped (SKIP_SMOKE=1)" >&2
	elif ! smoke=$("$out" version 2>&1); then
		fail_target "$TARGET does not run on this host: $smoke"
		return 0
	else
		record_fact "target=$TARGET smoke=ok smoke_output=\"$(printf '%s' "$smoke" | head -1)\""
		smoke_work="$TMPD/smoke"
		# The smoke driver refuses a caller-owned directory that is not 0700: it
		# writes a private CA there and will not carry on with a wider one. The mode
		# is therefore set explicitly rather than left to the ambient umask, which is
		# 022 on this host and is not ours to assume.
		mkdir -m 0700 -p "$smoke_work"
		# The log lives next to the work directory, not inside it: the driver
		# requires the directory it is handed to be empty, and a log file created
		# before it starts would be a caller-owned entry it must refuse.
		if smoke_qr "$out" "$smoke_work" >"$TMPD/smoke.log" 2>&1; then
			record_fact "target=$TARGET smoke_qr=ok review_tls=yes review_rejection_retained=yes review_evidence_backup=yes flow=\"init,scope add,scan,rule import,rule preview,rule activate,plan create,plan apply,quarantine list,quarantine restore,quarantine list\" mixed_batch_flow=\"scope add,scan,cleanup setup,rule import,rule enable,run,quarantine list,quarantine restore,cleanup report\" mixed_batch=\"deleted=1 observed=1 kept=1\" source_gone_after_apply=yes restored_bytes_match=yes"
		else
			step=$?
			fail_target "$TARGET failed the quarantine/restore smoke (step $step); see the log"
			sed -n '1,60p' "$TMPD/smoke.log" >&2 || true
			if grep -q 'openat2 unavailable\|function not implemented' "$TMPD/smoke.log"; then
				# The artifact is fine; the runtime cannot execute it. Saying so is
				# the difference between "our build is broken" and "this host cannot
				# run what it just built", and it is still a failed release.
				echo "       this runtime cannot execute the artifact: openat2 is not available to it," >&2
				echo "       so the release has to be built on a native $TARGET host" >&2
			fi
			return 0
		fi
	fi

	BUILT=$out
}

BUILT=""
build_target

echo "version=$VERSION commit=$COMMIT target=$TARGET built=$STAMP mode=$MODE"

if [ "$FAILED" -eq 1 ]; then
	fail_release "release is incomplete: $TARGET produced no acceptable artifact"
fi
if [ -z "$BUILT" ] || [ ! -s "$BUILT" ]; then
	# "Attempted" is not "produced": a step that returns 0 without leaving an
	# artifact is still a missing target.
	fail_release "no artifact for $TARGET"
fi

# A checksum file is worthless if it can be empty, partial or stale, so it is
# generated from exactly the artifact built here and then verified again.
SHA=""
if command -v shasum >/dev/null 2>&1; then
	SHA="shasum -a 256"
elif command -v sha256sum >/dev/null 2>&1; then
	SHA="sha256sum"
fi
if [ -z "$SHA" ]; then
	fail_release "no SHA-256 tool found; refusing to produce unverified artifacts"
fi

name=$(basename "$BUILT")
# The old "ls dustkeeper-*" glob is gone on purpose: a leftover artifact from an
# earlier run must not slip into the checksum file and get published as part of
# this build.
rm -f "$OUT/SHA256SUMS"
if ! (cd "$OUT" && $SHA "$name" >SHA256SUMS); then
	fail_release "could not write $OUT/SHA256SUMS"
fi
if ! (cd "$OUT" && $SHA -c SHA256SUMS >/dev/null); then
	fail_release "$OUT/SHA256SUMS does not verify"
fi
if [ "$(wc -l <"$OUT/SHA256SUMS" | tr -d ' ')" != "1" ]; then
	fail_release "SHA256SUMS does not cover exactly one artifact"
fi
checksum=$(cut -d' ' -f1 "$OUT/SHA256SUMS")

if [ "$MODE" = release ]; then
	# Record what was produced, under which policy, on which host, with which
	# checksum, and which checks this artifact actually ran. The libc line is
	# written only after the static-ELF proof was recorded: the label describes the
	# artifact in this build, it is not a constant.
	if [ "$HOST_OS" = "linux" ]; then
		if ! grep -q "^target=$TARGET .*linkage=static" "$FACTS_FILE"; then
			fail_release "release is incomplete, libc policy unproved: $TARGET"
		fi
		# The proof line names the inputs it was read from, so a reader can go and
		# look at them; it is not a sentence about the policy in general.
		libc_line="linux_libc=musl-static"
		libc_proof="linux_libc_proof=link inputs name musl (see startfile and musl_libdir above); artifact has no PT_INTERP and no DT_NEEDED"
	else
		libc_line="linux_libc=not-applicable"
		libc_proof="linux_libc_proof=not-applicable (this artifact is Mach-O)"
	fi
	# The manifest is written to a temporary name and moved into place only once it
	# is complete: a reader must never find a half-written manifest in dist.
	if ! {
		echo "# release manifest: one key=value per record"
		echo "# artifact and toolchain facts below were read during this build"
		echo "version=$VERSION"
		echo "commit=$COMMIT"
		echo "built=$STAMP"
		echo "host=$HOST_OS-$HOST_ARCH"
		echo "target=$TARGET"
		echo "supported_targets=$RELEASE_TARGETS"
		echo "build_env=${DUSTKEEPER_BUILD_ENV:-host}"
		echo "go=$(go version 2>/dev/null || echo unknown)"
		echo "$libc_line"
		echo "$libc_proof"
		echo "artifact=$name"
		echo "sha256=$checksum"
		cat "$FACTS_FILE"
	} >"$OUT/release-manifest.txt.tmp"; then
		rm -f "$OUT/release-manifest.txt.tmp"
		fail_release "could not write the release manifest"
	fi
	mv "$OUT/release-manifest.txt.tmp" "$OUT/release-manifest.txt"
fi

echo "artifacts in $OUT"
ls -1 "$OUT"
