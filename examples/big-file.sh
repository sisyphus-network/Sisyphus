#!/usr/bin/env bash
# Makes a large text file for timing jobs, by repeating the sample text.
#
#   examples/big-file.sh [megabytes] [output-file]
#
# Needs no node. Prints the path of the file it wrote.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)

megabytes=${1:-50}
out=${2:-${TMPDIR:-/tmp}/sisyphus-sample-${megabytes}mb.txt}

size=$(wc -c <"$HERE/boulder.txt")
copies=$((megabytes * 1024 * 1024 / size + 1))
: >"$out"
for _ in $(seq "$copies"); do
	cat "$HERE/boulder.txt"
done >>"$out"
echo "$out"
