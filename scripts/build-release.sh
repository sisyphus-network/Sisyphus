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

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
	os=${target%/*}
	arch=${target#*/}
	name="sisyphusd-$version-$os-$arch"
	if [ "$os" = windows ]; then
		name="$name.exe"
	fi
	echo "building $name"
	# No cgo, so the binaries are static and build without a C toolchain
	# for the target.
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" \
		-o "$out/$name" ./apps/sisyphusd
done

(cd "$out" && sha256sum sisyphusd-* >SHA256SUMS)
