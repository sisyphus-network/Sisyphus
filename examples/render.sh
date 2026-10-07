#!/usr/bin/env bash
# Renders a picture of the Mandelbrot set on the pool: the picture is cut
# into strips, each task renders one in a container, and the strips are
# put back together here.
#
#   examples/render.sh [strips] [width] [height] [file]
#
# The defaults are 8 strips of a 1200 by 800 picture, written to
# mandelbrot.pgm. At least one worker of the pool must have been started
# with --containers. The picture is a PGM, which most image viewers open.
#
# Each task learns which strip is its own from SISYPHUS_TASK_INDEX and
# SISYPHUS_TASK_COUNT, and leaves its rows in /output/rows, which the pool
# stores. Nothing here is special to pictures: it is the shape of any job
# that is one program run over parts of a whole.
set -euo pipefail
. "$(dirname "$0")/common.sh"

strips=${1:-8}
width=${2:-1200}
height=${3:-800}
file=${4:-mandelbrot.pgm}
if ! command -v python3 >/dev/null 2>&1; then
	echo "this example needs python3 to read the job's result" >&2
	exit 1
fi

# The program each task runs: its share of the rows, one number a pixel.
render='BEGIN {
	first = int(H * I / N); last = int(H * (I + 1) / N)
	for (y = first; y < last; y++) {
		line = ""
		for (x = 0; x < W; x++) {
			cr = -2.2 + 3.2 * x / W; ci = -1.1 + 2.2 * y / H
			zr = 0; zi = 0
			for (n = 0; n < 255 && zr * zr + zi * zi < 4; n++) {
				t = zr * zr - zi * zi + cr; zi = 2 * zr * zi + ci; zr = t
			}
			line = line (255 - n) " "
		}
		print line
		if ((y - first) % 20 == 0) printf "row %d of %d\n", y - first, last - first > "/dev/stderr"
	}
}'
params=$(W=$width H=$height RENDER=$render python3 -c '
import json, os
command = "awk -v W=%s -v H=%s -v I=$SISYPHUS_TASK_INDEX -v N=$SISYPHUS_TASK_COUNT \"$RENDER\" > /output/rows" % (os.environ["W"], os.environ["H"])
print(json.dumps({"image": "alpine:3.20", "command": ["sh", "-c", command], "env": {"RENDER": os.environ["RENDER"]}}))')

echo "rendering $width by $height in $strips strips"
out=$(node job submit --workload container --tasks "$strips" --params "$params")
echo "$out" | grep -v '^{' | tail -1

# The result names each strip's rows by CID, in order.
{
	printf 'P2\n%s %s\n255\n' "$width" "$height"
	for cid in $(echo "$out" | grep '^{' | python3 -c '
import json, sys
for task in sorted(json.load(sys.stdin)["tasks"], key=lambda t: t["index"]):
    print(task["files"]["rows"])'); do
		node blob get "$cid"
	done
} >"$file"
echo "wrote $file"
