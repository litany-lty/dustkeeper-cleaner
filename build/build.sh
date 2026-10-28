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
#   1. the compiler says which libc it links (<cc> -dumpmachine names musl, or its
#      own spec/include paths do), and its version is recorded next to the facts;
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
  DUSTKEEPER_BUILD_ENV=...  how to describe the build environment in the release
                       manifest, e.g. "docker:golang@sha256:..." for a pinned
                       builder image. Recorded, not interpreted.
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

HOST_OS=$(go env GOOS)
HOST_ARCH=$(go env GOARCH)
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
record_fact() {
	printf '%s\n' "$1" >>"$FACTS_FILE"
}

TMPD=$(mktemp -d "${TMPDIR:-/tmp}/dustkeeper-build.XXXXXX")
trap 'rm -rf "$TMPD"' EXIT

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
	if ! go build -trimpath -o "$VERIFY" ./build/verify-artifact; then
		echo "error: could not build the artifact verifier; refusing to produce unchecked artifacts" >&2
		exit 1
	fi
fi

# json_str reads one string field out of an envelope. It is deliberately narrow:
# the smoke only needs three identifiers, and adding a JSON parser to a shell
# script would be a new dependency to keep working.
json_str() {
	sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1
}

# smoke_qr exercises the two actions the product is for, on the artifact that will
# be published: register a directory, index it, plan a quarantine, apply it, list
# the vault and restore the object. It runs in a temporary tree and a temporary
# state directory, so it cannot touch anything of the operator's.
#
# It proves the artifact starts, opens its own database, performs a reversible
# move and can undo it. It is not a substitute for the source tests: those use a
# controlled clock and cover the refusal paths, and the manifest says which checks
# this artifact actually ran.
smoke_qr() {
	art=$1
	work=$2
	mkdir -p "$work/tree" "$work/state" "$work/config"
	printf 'smoke payload\n' >"$work/tree/old.zip"
	# The rule's minimum age is a day (the schema refuses anything shorter), so the
	# file has to look older than that. touch -t is POSIX on both targets.
	touch -t 202001010000 "$work/tree/old.zip"
	rule=$work/rule.json
	cat >"$rule" <<'JSON'
{"dsl_version":1,"scope_id":"smoke","action":"quarantine",
 "match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":1073741824,
          "relative_dir_prefixes":[],"basename_patterns":["*.zip"]},
 "except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}
JSON

	# One environment for every step: the state directory, not a flag, is what the
	# binary uses, and the smoke must prove that path works.
	run() {
		DUSTKEEPER_STATE_DIR="$work/state" DUSTKEEPER_CONFIG_DIR="$work/config" "$art" "$@"
	}

	# step runs one call and, when it fails, shows what the tool actually said.
	# Without this a failed smoke reports an empty log whenever the error is
	# reported as JSON on stdout (which is what the tool does for --json).
	#
	# The names are prefixed because this runs inside build_target's scope: a
	# plain `out` here would overwrite the artifact path the caller still needs.
	step() {
		s_code=$1
		s_out=$2
		shift 2
		if ! run "$@" >"$s_out" 2>>"$work/log"; then
			echo "smoke: step $s_code failed: dustkeeper $*" >&2
			sed -n '1,40p' "$s_out" >&2 || true
			return "$s_code"
		fi
		return 0
	}

	step 10 "$work/init.json" init --json || return $?
	step 11 "$work/scope.json" scope add --path "$work/tree" --kind managed_disposable --name smoke --json || return $?
	step 12 "$work/scan.json" scan --scope smoke --json || return $?
	step 13 "$work/import.json" rule import --file "$rule" --id smoke-rule --json || return $?
	step 14 "$work/preview.json" rule preview --id smoke-rule --revision 1 --json || return $?
	digest=$(json_str review_digest <"$work/preview.json")
	if [ -z "$digest" ]; then
		echo "smoke: the preview returned no review_digest" >&2
		return 14
	fi
	step 15 "$work/activate.json" rule activate --id smoke-rule --revision 1 --digest "$digest" --json || return $?
	step 16 "$work/plan.json" plan create --rule smoke-rule --json || return $?
	plan_id=$(json_str plan_id <"$work/plan.json")
	plan_digest=$(json_str digest <"$work/plan.json")
	if [ -z "$plan_id" ] || [ -z "$plan_digest" ]; then
		echo "smoke: the plan returned no id or digest" >&2
		return 16
	fi
	step 17 "$work/apply.json" plan apply --id "$plan_id" --digest "$plan_digest" --json || return $?

	step 18 "$work/list.json" quarantine list --json || return $?
	item=$(json_str item_id <"$work/list.json")
	if [ -z "$item" ]; then
		echo "smoke: nothing was listed after a quarantine was applied" >&2
		return 18
	fi
	if ! grep -q '"restorable"[[:space:]]*:[[:space:]]*true' "$work/list.json"; then
		echo "smoke: the quarantined object was not restorable" >&2
		return 18
	fi
	step 19 "$work/restore.json" quarantine restore --object "$item" --json || return $?
	if [ ! -f "$work/tree/old.zip" ]; then
		echo "smoke: the restore did not put the file back" >&2
		return 19
	fi
	return 0
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
		# Which libc will this compiler actually link? A compiler's real target is
		# what it says about its own spec and include search path, not what its
		# filename is: Debian's musl-gcc reports "x86_64-linux-gnu" from
		# -dumpmachine and is still musl, because it injects musl's specs, headers
		# and libc.a. So the evidence is taken from the two places the compiler
		# answers about itself:
		#
		#   * the include search list it prints for -v, read only from the block
		#     between "#include <...> search starts here:" and "End of search
		#     list." -- that block is where headers are really looked up;
		#   * the libc.a it would link (-print-file-name=libc.a).
		#
		# A path that merely mentions musl somewhere in the transcript is not
		# evidence: a banner line, or a -print-sysroot answer, can say anything
		# while the compiler links glibc. Only a musl path in the search list the
		# compiler really uses, or a musl libc.a, counts.
		probe_src="$TMPD/libc-probe.c"
		printf 'int main(void){return 0;}\n' >"$probe_src"
		probe_log="$TMPD/libc-probe.log"
		"$probe_cc" -v -c "$probe_src" -o "$TMPD/libc-probe.o" >"$probe_log" 2>&1 || true
		include_list=$(awk '/#include <\.\.\.> search starts here:/{f=1;next} /End of search list\./{f=0} f' \
			"$probe_log" 2>/dev/null || true)
		musl_include=$(printf '%s\n' "$include_list" | tr -s ' \t' '\n' | grep 'musl' | head -1 || true)
		libc_a=$("$probe_cc" -print-file-name=libc.a 2>/dev/null || echo libc.a)
		musl_libc=""
		case "$libc_a" in
		/*musl*) musl_libc=$libc_a ;;
		esac
		musl_evidence=${musl_include:-$musl_libc}
		proved=0
		case "$triple" in
		*musl*) proved=1 ;;
		esac
		if [ -n "$musl_evidence" ]; then
			proved=1
		fi
		if [ "$proved" = 0 ]; then
			# A glibc static binary is still a glibc binary: it must never be
			# labelled musl-static, and a release refuses it outright.
			if [ "$MODE" = release ] || [ "${ALLOW_UNPROVED_LIBC:-0}" != "1" ]; then
				fail_target "$TARGET was built with $probe_cc, which targets '$triple' and links $libc_a, not musl"
				return 0
			fi
			if ! facts=$("$VERIFY" -file "$out" -format elf -machine "$HOST_ARCH" 2>&1); then
				fail_target "$TARGET is not a readable $HOST_ARCH ELF: $facts"
				return 0
			fi
			echo "dev: $TARGET linkage is unproved (compiler targets '$triple' and links $libc_a); this artifact is not a musl static build" >&2
			record_fact "target=$TARGET format=elf arch=$HOST_ARCH linkage=unproved toolchain=$triple compiler=$probe_cc compiler_version=\"$ccver\" libc_a=$libc_a include_search=\"$(printf '%s' "$include_list" | tr '\n' ' ' | sed 's/  */ /g')\" build_env=${DUSTKEEPER_BUILD_ENV:-host} artifact=[$facts]"
		else
			if ! facts=$("$VERIFY" -file "$out" -format elf -machine "$HOST_ARCH" -require static 2>&1); then
				fail_target "$TARGET is not a static $HOST_ARCH ELF: $facts"
				return 0
			fi
			record_fact "target=$TARGET format=elf arch=$HOST_ARCH linkage=static toolchain=$triple compiler=$probe_cc compiler_version=\"$ccver\" sysroot=${sysroot:-none} musl_evidence=$musl_evidence build_env=${DUSTKEEPER_BUILD_ENV:-host} artifact=[$facts]"
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
		mkdir -p "$smoke_work"
		if smoke_qr "$out" "$smoke_work" >"$smoke_work/log" 2>&1; then
			record_fact "target=$TARGET smoke_qr=ok flow=\"init,scope add,scan,rule import,rule preview,rule activate,plan create,plan apply,quarantine list,quarantine restore\""
		else
			step=$?
			fail_target "$TARGET failed the quarantine/restore smoke (step $step); see the log"
			sed -n '1,60p' "$smoke_work/log" >&2 || true
			if grep -q 'openat2 unavailable\|function not implemented' "$smoke_work/log"; then
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
	echo "release is incomplete: $TARGET produced no acceptable artifact" >&2
	exit 1
fi
if [ -z "$BUILT" ] || [ ! -s "$BUILT" ]; then
	# "Attempted" is not "produced": a step that returns 0 without leaving an
	# artifact is still a missing target.
	echo "error: no artifact for $TARGET" >&2
	exit 1
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
	echo "error: no SHA-256 tool found; refusing to produce unverified artifacts" >&2
	exit 1
fi

name=$(basename "$BUILT")
# The old "ls dustkeeper-*" glob is gone on purpose: a leftover artifact from an
# earlier run must not slip into the checksum file and get published as part of
# this build.
rm -f "$OUT/SHA256SUMS"
if ! (cd "$OUT" && $SHA "$name" >SHA256SUMS); then
	echo "error: could not write $OUT/SHA256SUMS" >&2
	exit 1
fi
if ! (cd "$OUT" && $SHA -c SHA256SUMS >/dev/null); then
	echo "error: $OUT/SHA256SUMS does not verify" >&2
	exit 1
fi
if [ "$(wc -l <"$OUT/SHA256SUMS" | tr -d ' ')" != "1" ]; then
	echo "error: SHA256SUMS does not cover exactly one artifact" >&2
	exit 1
fi
checksum=$(cut -d' ' -f1 "$OUT/SHA256SUMS")

if [ "$MODE" = release ]; then
	# Record what was produced, under which policy, on which host, with which
	# checksum, and which checks this artifact actually ran. The libc line is
	# written only after the static-ELF proof was recorded: the label describes the
	# artifact in this build, it is not a constant.
	if [ "$HOST_OS" = "linux" ]; then
		if ! grep -q "^target=$TARGET .*linkage=static" "$FACTS_FILE"; then
			echo "release is incomplete, libc policy unproved: $TARGET" >&2
			exit 1
		fi
		libc_line="linux_libc=musl-static"
		libc_proof="linux_libc_proof=compiler names musl; artifact has no PT_INTERP and no DT_NEEDED"
	else
		libc_line="linux_libc=not-applicable"
		libc_proof="linux_libc_proof=not-applicable (this artifact is Mach-O)"
	fi
	{
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
	} >"$OUT/release-manifest.txt"
fi

echo "artifacts in $OUT"
ls -1 "$OUT"
