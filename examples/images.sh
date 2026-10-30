#!/usr/bin/env bash
# Edits a batch of pictures on the pool: each is stored, a run of them goes
# to each worker, Real-ESRGAN upscales them if asked, libvips fits them to
# a size and saves them in a format, and a zip of them all is fetched.
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
#
# UPSCALE (2, 3 or 4) has Real-ESRGAN make each picture that many times
# bigger first, then fits and saves it as above; MODEL picks its network:
# realesrgan-x4plus (the default, for photos), realesrgan-x4plus-anime
# (for drawings) or realesr-animevideov3 (small and fast, for frames of
# anime). GPU=true runs it on a worker's graphics card; without one it
# runs on a worker whose processor has AVX2, and is far slower. Upscaled
# pictures keep no metadata, so KEEP_METADATA cannot be set with UPSCALE.
set -euo pipefail

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
# Keep in step with imagesMostPictures in packages/runtime/images.go.
max_pictures=256
if [ ${#pictures[@]} -gt "$max_pictures" ]; then
	echo "too many pictures: at most $max_pictures may be edited in one job" >&2
	exit 1
fi
for picture in "${pictures[@]}"; do
	[ -f "$picture" ] || { echo "no such file: $picture" >&2; exit 1; }
done

. "$(dirname "$0")/common.sh"

inputs=
for picture in "${pictures[@]}"; do
	cid=$(node blob put "$picture")
	echo "stored $picture as $cid"
	inputs=${inputs:+$inputs,}\"$cid\"
done

params="{\"inputs\":[$inputs],\"width\":${WIDTH:-0},\"height\":${HEIGHT:-0},\"format\":\"${FORMAT:-same}\",\"quality\":${QUALITY:-85},\"keep_metadata\":${KEEP_METADATA:-false},\"upscale\":${UPSCALE:-0},\"model\":\"${MODEL:-}\",\"gpu\":${GPU:-false},\"tasks\":${TASKS:-0}}"
upscaled=
if [ "${UPSCALE:-0}" != 0 ]; then upscaled=", upscaled by $UPSCALE"; fi
echo "editing ${#pictures[@]} pictures on the pool$upscaled"
out=$(node job submit --workload images --params "$params")
echo "$out" | sed -n 's/^job \([0-9a-f]*\) succeeded in \(.*\)/  job \1 took \2/p'
result=$(echo "$out" | tail -n 1)
echo "$result" | sed -n 's/.*"made":\([0-9]*\),"failed":\([0-9]*\).*/  made \1, could not make \2/p'
archive=$(echo "$result" | sed 's/.*"archive":"\([^"]*\)".*/\1/')

node blob get -o "$output" "$archive"
echo "wrote $output ($(wc -c <"$output") bytes)"
