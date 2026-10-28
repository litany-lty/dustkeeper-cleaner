#!/bin/sh
# Build the linux-amd64 release artifact inside the pinned builder image.
#
# Why a script at all: the Linux artifact must be a musl static ELF, and the
# machine this repository is developed on is a macOS one. Rather than trusting
# whatever compiler happens to be installed, the release is produced by exactly one
# image, named by digest, on exactly one architecture.
#
# Three rules, all of them load-bearing:
#
#   - the image is pinned by digest. A tag is a moving name, and "it was golang:1.26
#     last week" is not a record of what produced an artifact.
#   - the target is linux/amd64, selected with --platform. That is the only Linux
#     release target. Selecting the platform also means an arm64 workstation runs
#     the amd64 image, instead of quietly producing an arm64 binary and calling it
#     the Linux release.
#   - no compiler comes from outside. CC is set to musl-gcc inside the container and
#     any inherited CC is dropped, so the artifact's libc cannot be swapped by the
#     caller. build/build.sh still proves it.
#
# One thing the container forces: the quarantine/restore smoke has to run on a
# filesystem the product accepts (it refuses to move files on anything it has not
# validated), and the container's own /tmp is overlayfs. The smoke therefore runs in
# a named volume, which the container engine backs with ext4, and TMPDIR says so.
#
# The second, harder requirement is that the runtime can actually execute the
# artifact: the product resolves paths with openat2(RESOLVE_BENEATH|NO_SYMLINKS) and
# moves with renameat2(RENAME_NOREPLACE). Emulated amd64 containers on an arm64 host
# answer ENOSYS for openat2 (Rosetta), so the smoke fails there and this script fails
# with it -- correctly, and with that reason printed. linux-amd64 releases are
# therefore produced on a native x86_64 host: this machine if it is one, otherwise
# the CI runner.
#
# Usage:
#   ./build/build-linux-release.sh
#
# Requires Docker. Network access is needed once for the image and for musl-tools
# inside it; the Go module cache is reused through a named volume.
#
# The verification below reads dist/release-manifest.txt, which build.sh wrote from
# facts it read during this build. Nothing here globs the dist directory: an
# artifact left over from an earlier run is not this release.
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)

# Pinned builder: golang:1.26-bookworm, linux/amd64 manifest.
DEFAULT_IMAGE="golang:1.26-bookworm@sha256:88750ccdd0f2a77fcc00a14b1a48d6409bd8ddb5d7c2dd6ec0b4a2e916daf9d9"
IMAGE=${DUSTKEEPER_BUILDER_IMAGE:-$DEFAULT_IMAGE}

case "$IMAGE" in
*@sha256:*) ;;
*)
	echo "error: the builder image must be pinned by digest, got '$IMAGE'" >&2
	echo "       e.g. golang:1.26-bookworm@sha256:<manifest digest>" >&2
	exit 1
	;;
esac

echo "builder image: $IMAGE"
echo "target:        linux/amd64"

# The smoke's working tree has to live on a filesystem the product validates. A
# named volume is the portable way to get one inside a container.
SMOKE_VOLUME=${DUSTKEEPER_SMOKE_VOLUME:-dustkeeper-release-smoke}
docker volume create "$SMOKE_VOLUME" >/dev/null

docker run --rm \
	--platform linux/amd64 \
	-v "$repo":/src \
	-v dustkeeper-go-cache:/gocache \
	-v "$SMOKE_VOLUME":/smoke \
	-e DK_IMAGE="$IMAGE" \
	"$IMAGE" sh -c '
set -eu
cd /src

apt-get update -qq
apt-get install -y -qq musl-tools binutils >/dev/null

export GOCACHE=/gocache
# The smoke tree and the state directory it uses must be on ext4/xfs/apfs: scope
# registration refuses anything else, and the /tmp of the container is overlayfs.
export TMPDIR=/smoke/tmp
rm -rf "$TMPDIR"
mkdir -p "$TMPDIR"
# The only compiler this release uses. Anything the caller exported is discarded:
# the libc policy is not a caller option.
unset CC_linux_amd64 CC_linux_arm64 CGO_CFLAGS CGO_LDFLAGS
export CC=musl-gcc
# Keep the toolchain versions next to the artifact. They are recorded, not inferred
# from the image name.
go_version=$(go version)
musl_version=$(musl-gcc --version | head -1)
export DUSTKEEPER_BUILD_ENV="docker:$DK_IMAGE platform=linux/amd64 $go_version / $musl_version"

echo "go:   $go_version"
echo "musl: $musl_version"
echo

./build/build.sh --release

manifest=dist/release-manifest.txt
if [ ! -f "$manifest" ]; then
	echo "error: build.sh produced no release manifest, so nothing here is verified" >&2
	exit 1
fi

artifact=$(sed -n "s/^artifact=//p" "$manifest")
want_sha=$(sed -n "s/^sha256=//p" "$manifest")
if [ -z "$artifact" ] || [ -z "$want_sha" ]; then
	echo "error: the manifest does not name the artifact and its checksum" >&2
	cat "$manifest" >&2
	exit 1
fi
if [ ! -s "dist/$artifact" ]; then
	echo "error: the manifest names $artifact, which is not in dist" >&2
	exit 1
fi

echo
echo "=== checksum (recomputed here, not read back) ==="
got_sha=$(sha256sum "dist/$artifact" | cut -d" " -f1)
if [ "$got_sha" != "$want_sha" ]; then
	echo "error: $artifact hashes to $got_sha, the manifest says $want_sha" >&2
	exit 1
fi
echo "$artifact sha256=$got_sha (matches the manifest)"

if ! (cd dist && sha256sum -c SHA256SUMS >/dev/null); then
	echo "error: SHA256SUMS does not verify" >&2
	exit 1
fi
echo "SHA256SUMS verifies"

echo
echo "=== independent ELF check (readelf, not build.sh helpers) ==="
machine=$(readelf -h "dist/$artifact" | sed -n "s/.*Machine:[[:space:]]*//p")
interp=$(readelf -l "dist/$artifact" | grep -c INTERP || true)
needed=$(readelf -d "dist/$artifact" 2>/dev/null | grep -c NEEDED || true)
echo "$artifact: machine=$machine interp=$interp needed=$needed"
case "$machine" in
*"X86-64"*) ;;
*)
	echo "error: $artifact is not an x86-64 ELF (machine=$machine)" >&2
	exit 1
	;;
esac
if [ "$interp" != "0" ] || [ "$needed" != "0" ]; then
	echo "error: $artifact is not statically linked (interp=$interp needed=$needed)" >&2
	exit 1
fi

echo
echo "=== smoke: the artifact in this container ==="
# The build script already ran `version` and the quarantine/restore cycle on this
# artifact; running it again here proves the binary in dist is the one that ran.
version_out=$(./"dist/$artifact" version)
echo "$artifact: $version_out"

echo
echo "=== release manifest ==="
cat "$manifest"
'
