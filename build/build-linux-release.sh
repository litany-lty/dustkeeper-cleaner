#!/bin/sh
# Build the Linux release targets for real, inside a container, and prove what
# came out.
#
# Why a script: `build/build.sh` refuses to write a release manifest unless each
# Linux target was compiled by musl and the artifact is statically linked, with
# both facts recorded. Reproducing that means having musl-gcc on the build host,
# which a macOS development machine does not. This wraps the one command that
# does, so the claim in docs/compliance.md can be re-run instead of remembered.
#
# Usage:
#   ./build/build-linux-release.sh            # the host architecture (musl static)
#   ./build/build-linux-release.sh arm64      # one target, explicitly
#
# It defaults to the container's own architecture on purpose: `musl-gcc` compiles
# for the architecture it was built for, so asking it for the other one fails in
# the C compiler with "-m64: unrecognized command-line option" (or the mirror
# image with -march). A real cross build needs a cross musl toolchain, which this
# script does not pretend to have.
#
# Requires Docker. Network access is needed once to install musl-tools inside the
# container; the Go module cache is reused through the named volume.
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
arches=${*:-auto}

docker run --rm \
	-v "$repo":/src \
	-v dustkeeper-go-cache:/gocache \
	-e DK_ARCHES="$arches" \
	golang:1.26-bookworm sh -c '
set -e
case "$DK_ARCHES" in
auto)
	case "$(uname -m)" in
	x86_64) DK_ARCHES=amd64 ;;
	aarch64|arm64) DK_ARCHES=arm64 ;;
	*) echo "unsupported container architecture $(uname -m)"; exit 2 ;;
	esac
	echo "host architecture $(uname -m): building linux/$DK_ARCHES only"
	;;
esac

apt-get update -qq
apt-get install -y -qq musl-tools >/dev/null
export GOCACHE=/gocache
export DUSTKEEPER_BUILD_ENV="docker golang:1.26-bookworm $(uname -m)"
cd /src

for arch in $DK_ARCHES; do
	export CC_linux_${arch}=musl-gcc
done

# Build first: build.sh is the single source of truth for the targets, the musl
# proof, the artifact check and the manifest gate.
./build/build.sh --release || status=$?
status=${status:-0}

echo
echo "=== artifacts ==="
ls -l dist | sed -n "1,20p"

echo
echo "=== facts ==="
cat dist/build-facts.txt 2>/dev/null || echo "no build-facts.txt"

echo
echo "=== artifact check (independent of build.sh) ==="
for f in dist/dustkeeper-*; do
	case "$f" in
	*.txt|*.json|*SUMS*) continue ;;
	esac
	# Only ELF files can be inspected here; a Mach-O artifact is checked on the
	# machine that built it.
	if head -c 4 "$f" | grep -q ELF 2>/dev/null; then
		machine=$(readelf -h "$f" | sed -n "s/.*Machine:[[:space:]]*//p")
		interp=$(readelf -l "$f" | grep -c INTERP || true)
		needed=$(readelf -d "$f" 2>/dev/null | grep -c NEEDED || true)
		echo "$f: machine=$machine interp=$interp needed=$needed"
	fi
done

echo
echo "=== smoke (same host, so only the container architecture runs) ==="
for f in dist/dustkeeper-*; do
	case "$f" in
	*.txt|*.json|*SUMS*) continue ;;
	esac
	if "$f" version >/dev/null 2>&1; then
		echo "$(basename "$f"): $("$f" version)"
	else
		echo "$(basename "$f"): not runnable on $(uname -m) - not a failure, but not a run record either"
	fi
done

exit $status
'
