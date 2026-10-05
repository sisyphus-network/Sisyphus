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

# Each node keeps its key and data in a directory of its own.
"$BIN" run --listen "$ADDR" --data-dir "$LOGS/alpha" --name alpha --slots 4 >"$LOGS/alpha.log" 2>&1 &
pids+=($!)
# Commands below are run as alpha's owner, from alpha's data directory.
alpha() { "$BIN" "$@" --addr "$ADDR" --data-dir "$LOGS/alpha"; }
for _ in $(seq 50); do
	alpha nodes >/dev/null 2>&1 && break
	sleep 0.1
done

# A worker joins with an invitation from the coordinator, which tells it
# which node to trust and gets it admitted.
for node in beta gamma; do
	"$BIN" run --role worker --coordinator "$ADDR" --join "$(alpha pool invite)" \
		--data-dir "$LOGS/$node" --name "$node" --slots 4 >"$LOGS/$node.log" 2>&1 &
	pids+=($!)
done

# Wait for all three workers to be connected.
for _ in $(seq 50); do
	if [ "$(alpha nodes 2>/dev/null | grep -c primes)" = 3 ]; then
		break
	fi
	sleep 0.1
done

echo "== nodes"
alpha nodes
echo
echo "== counting primes below 3 billion, split across the pool"
alpha job submit --params '{"from":0,"to":3000000000}'
echo
echo "== the same job as a single task on one worker"
alpha job submit --mode full-worker --params '{"from":0,"to":3000000000}'

echo
echo "== counting words in a stored file"
# About 40 MB of text built from the repository's own README.
for _ in $(seq 4000); do cat README.md; done >"$LOGS/corpus.txt"
input=$("$BIN" blob put --addr "$ADDR" --data-dir "$LOGS/alpha" "$LOGS/corpus.txt")
echo "input stored as $input"
alpha job submit --workload wordcount --params "{\"input\":\"$input\"}" | tee "$LOGS/wordcount.out"
output=$(tail -n 1 "$LOGS/wordcount.out" | sed 's/.*"output":"\([^"]*\)".*/\1/')
echo
echo "== the ten most frequent words, fetched from the result blob $output"
"$BIN" blob get --addr "$ADDR" --data-dir "$LOGS/alpha" "$output" | head -n 10
