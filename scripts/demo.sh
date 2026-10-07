#!/usr/bin/env bash
# Runs a three-node pool on this machine in one terminal: a coordinator that
# is also a worker, two more workers, and a job split across them.
set -euo pipefail
cd "$(dirname "$0")/.."

ADDR=${ADDR:-127.0.0.1:7799}
BIN=bin/sisyphusd
LOGS=$(mktemp -d)

go build -o "$BIN" ./apps/sisyphusd

pids=()
cleanup() {
	kill "${pids[@]}" 2>/dev/null || true
	wait 2>/dev/null || true
	rm -rf "$LOGS"
}
trap cleanup EXIT

"$BIN" run --listen "$ADDR" --node-id alpha --slots 4 >"$LOGS/alpha.log" 2>&1 &
pids+=($!)
for node in beta gamma; do
	"$BIN" run --role worker --coordinator "$ADDR" --node-id "$node" --slots 4 >"$LOGS/$node.log" 2>&1 &
	pids+=($!)
done

# Wait for all three workers to join.
for _ in $(seq 50); do
	if [ "$("$BIN" nodes --addr "$ADDR" 2>/dev/null | grep -c primes)" = 3 ]; then
		break
	fi
	sleep 0.1
done

echo "== nodes"
"$BIN" nodes --addr "$ADDR"
echo
echo "== counting primes below 3 billion, split across the pool"
"$BIN" job submit --addr "$ADDR" --params '{"from":0,"to":3000000000}'
echo
echo "== the same job as a single task on one worker"
"$BIN" job submit --addr "$ADDR" --mode full-worker --params '{"from":0,"to":3000000000}'
