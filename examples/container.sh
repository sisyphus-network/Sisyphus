#!/usr/bin/env bash
# Runs a container image on the pool, one copy for each task, and prints
# what each task printed.
#
#   examples/container.sh [tasks] [image] [command...]
#
# With no arguments it runs three copies of a small image, each of which
# says which copy it is. At least one worker of the pool must have been
# started with --containers.
set -euo pipefail
. "$(dirname "$0")/common.sh"

tasks=${1:-3}
image=${2:-alpine:3.20}
shift $(( $# > 2 ? 2 : $# ))
if [ $# -eq 0 ]; then
	set -- sh -c 'echo "task $SISYPHUS_TASK_INDEX of $SISYPHUS_TASK_COUNT on $(hostname)"'
fi

# The command, as a JSON list.
command=$(printf '%s\n' "$@" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read().splitlines()))' 2>/dev/null) || {
	echo "this example needs python3 to quote the command" >&2
	exit 1
}
params="{\"image\":\"$image\",\"command\":$command}"

echo "running $image in $tasks tasks"
out=$(node job submit --workload container --tasks "$tasks" --params "$params")
echo "$out" | grep -v '^{'
# The result lists, for each task, the CID of what it printed.
for cid in $(echo "$out" | grep -o '"stdout":"[^"]*"' | cut -d'"' -f4); do
	node blob get "$cid"
done
