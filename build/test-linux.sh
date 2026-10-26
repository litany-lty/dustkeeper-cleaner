#!/bin/sh
# Run the test suite on linux/amd64 against a real ext4 filesystem.
#
# The macOS development machine cannot run the Linux half of the suite, and a
# container's overlay filesystem is not what a Linux user runs on: Dustkeeper
# refuses a filesystem it has not validated, so every integration test aborts
# with UNSUPPORTED_FS on overlay. This script builds an ext4 loop image inside
# a privileged container and points TMPDIR at it, so the filesystem the tests
# see is the filesystem a Linux user has.
#
# Usage: ./build/test-linux.sh [-count=1] [-run Pattern]
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
args=${*:--count=1}

docker run --rm --privileged \
	-v "$repo":/src:ro \
	-v dustkeeper-go-cache:/gocache \
	-e DK_ARGS="$args" \
	golang:1.26-bookworm sh -c '
set -e
dd if=/dev/zero of=/disk.img bs=1M count=3072 status=none
mkfs.ext4 -q -F /disk.img
mkdir -p /ext4 && mount -o loop /disk.img /ext4
cp -a /src/. /ext4/
mkdir -p /ext4/tmp
cd /ext4
export GOCACHE=/gocache
export TMPDIR=/ext4/tmp
echo "filesystem: $(stat -f -c %T /ext4/tmp) on $(uname -m)"
# shellcheck disable=SC2086
go test ./... $DK_ARGS
'
