#!/usr/bin/env bash
# Builds sisyphusd for every supported platform.
#
#   scripts/build-release.sh <version> <output-dir>
#
# Produces one binary per platform, named sisyphusd-<version>-<os>-<arch>,
# and a SHA256SUMS file listing them.
set -euo pipefail
cd "$(dirname "$0")/.."

version=${1:?usage: build-release.sh <version> <output-dir>}
out=${2:?usage: build-release.sh <version> <output-dir>}
mkdir -p "$out"

# Every target here builds without a C toolchain. Only linux/amd64 is run by
# the test suite; the rest are compiled and otherwise untested. Not built:
# Solaris, illumos, AIX and Plan 9 (a dependency does not support them), and
# iOS and Android on x86, which need a platform SDK to link.
targets="
	linux/amd64 linux/arm64 linux/arm linux/riscv64
	darwin/amd64 darwin/arm64
	windows/amd64 windows/arm64
	freebsd/amd64 freebsd/arm64 openbsd/amd64 netbsd/amd64
	android/arm64
"

for target in $targets; do
	os=${target%/*}
	arch=${target#*/}
	name="sisyphusd-$version-$os-$arch"
	if [ "$os" = windows ]; then
		name="$name.exe"
	fi
	echo "building $name"
	# No cgo, so the binaries are static and build without a C toolchain
	# for the target.
	# GOARM only affects 32-bit ARM, where 7 covers the Raspberry Pi 2 on.
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=7 go build -trimpath \
		-ldflags "-s -w -X main.version=$version" \
		-o "$out/$name" ./apps/sisyphusd
done

(cd "$out" && sha256sum sisyphusd-* >SHA256SUMS)
