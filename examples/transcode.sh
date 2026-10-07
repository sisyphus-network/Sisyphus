#!/usr/bin/env bash
# Re-encodes a video on the pool: the video is stored, cut into stretches
# that the workers encode at the same time, and the result is fetched.
#
#   examples/transcode.sh <video> [height] [output]
#
# The result is H.264 video with AAC sound in an MPEG transport stream,
# written to <video's name>.ts unless an output is named. With a height,
# the picture is scaled to it. At least one worker of the pool must have
# been started with --containers; each builds the image it needs the first
# time, which takes a minute and the network.
#
# QUALITY (1 best to 51, default 23), PRESET (ultrafast to veryslow,
# default medium) and SEGMENTS (default: one for each free slot) may be
# set in the environment.
set -euo pipefail
. "$(dirname "$0")/common.sh"

if [ $# -lt 1 ] || [ ! -f "$1" ]; then
	echo "usage: examples/transcode.sh <video> [height] [output]" >&2
	exit 1
fi
video=$1
height=${2:-0}
output=${3:-$(basename "${video%.*}").ts}

echo "storing $video"
input=$(node blob put "$video")
echo "  as $input"

params="{\"input\":\"$input\",\"height\":$height,\"crf\":${QUALITY:-23},\"preset\":\"${PRESET:-medium}\",\"segments\":${SEGMENTS:-0}}"
echo "encoding on the pool"
out=$(node job submit --workload transcode --params "$params")
echo "$out" | sed -n 's/^job \([0-9a-f]*\) succeeded in \(.*\)/  job \1 took \2/p'
result=$(echo "$out" | tail -n 1)
cid=$(echo "$result" | sed 's/.*"output":"\([^"]*\)".*/\1/')
echo "  $result"

node blob get -o "$output" "$cid"
echo "wrote $output ($(wc -c <"$output") bytes, from $(wc -c <"$video"))"
echo "to put it in an MP4 without encoding again: ffmpeg -i $output -c copy ${output%.ts}.mp4"
