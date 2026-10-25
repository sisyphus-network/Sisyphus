#!/usr/bin/env bash
# Edits a batch of pictures on the pool: each is stored, a run of them goes
# to each worker, libvips fits them to a size and saves them in a format,
# and a zip of them all is fetched.
#
#   examples/images.sh <picture>... [-- output.zip]
#
# Pictures may be PNG, JPEG, WebP, TIFF or GIF. The zip is written to
# pictures.zip unless one is named after --. At least one worker of the
# pool must have been started with --containers; each builds the image it
# needs the first time, which takes a minute and the network.
#
# WIDTH and HEIGHT (pixels to fit within; 0, the default, leaves a side
# free), FORMAT (same, the default, or jpeg, png, webp, tiff, gif), QUALITY
# (1 to 100, default 85), KEEP_METADATA (true to keep EXIF and the like;
# by default it is taken out) and TASKS (default: left to the pool) may be
# set in the environment.
set -euo pipefail
. "$(dirname "$0")/common.sh"

pictures=()
output=pictures.zip
while [ $# -gt 0 ]; do
	case $1 in
	--) output=${2:?a file to write the zip to follows --}; shift 2 ;;
	*) pictures+=("$1"); shift ;;
	esac
done
if [ ${#pictures[@]} -eq 0 ]; then
	echo "usage: examples/images.sh <picture>... [-- output.zip]" >&2
	exit 1
fi

inputs=
for picture in "${pictures[@]}"; do
	[ -f "$picture" ] || { echo "no such file: $picture" >&2; exit 1; }
	cid=$(node blob put "$picture")
	echo "stored $picture as $cid"
	inputs=${inputs:+$inputs,}\"$cid\"
done

params="{\"inputs\":[$inputs],\"width\":${WIDTH:-0},\"height\":${HEIGHT:-0},\"format\":\"${FORMAT:-same}\",\"quality\":${QUALITY:-85},\"keep_metadata\":${KEEP_METADATA:-false},\"tasks\":${TASKS:-0}}"
echo "editing ${#pictures[@]} pictures on the pool"
out=$(node job submit --workload images --params "$params")
echo "$out" | sed -n 's/^job \([0-9a-f]*\) succeeded in \(.*\)/  job \1 took \2/p'
result=$(echo "$out" | tail -n 1)
echo "$result" | sed -n 's/.*"made":\([0-9]*\),"failed":\([0-9]*\).*/  made \1, could not make \2/p'
archive=$(echo "$result" | sed 's/.*"archive":"\([^"]*\)".*/\1/')

node blob get -o "$output" "$archive"
echo "wrote $output ($(wc -c <"$output") bytes)"
