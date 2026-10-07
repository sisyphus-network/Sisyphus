#!/usr/bin/env bash
# Runs a pool on this machine and leaves it running for you to use: one
# coordinator and two workers, each a separate process with its own key and
# data. Stop it with Ctrl-C.
#
#   examples/local-pool.sh [--kubo]
#
# With --kubo every node runs Kubo beside itself and the three form a
# private IPFS network; the ipfs program must be installed.
#
# The pool's files go in a directory of their own (POOL_DIR, default
# ./sisyphus-pool) and are kept between runs, so stored data and joined
# workers are still there next time.
set -euo pipefail
cd "$(dirname "$0")/.."

kubo=()
if [ "${1:-}" = "--kubo" ]; then
	# A random port for Kubo, so that two pools on one machine do not collide.
	kubo=(--kubo --swarm-port 0)
fi
ADDR=${ADDR:-127.0.0.1:7700}
POOL_DIR=${POOL_DIR:-$PWD/sisyphus-pool}
mkdir -p "$POOL_DIR"

go build -o bin/sisyphusd ./apps/sisyphusd
BIN=bin/sisyphusd

pids=()
trap 'echo; echo "stopping the pool"; kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true' EXIT

"$BIN" run --role coordinator --listen "$ADDR" --data-dir "$POOL_DIR/alpha" --name alpha "${kubo[@]}" \
	>"$POOL_DIR/alpha.log" 2>&1 &
pids+=($!)
alpha() { "$BIN" "$@" --addr "$ADDR" --data-dir "$POOL_DIR/alpha"; }
for _ in $(seq 300); do
	alpha nodes >/dev/null 2>&1 && break
	sleep 0.1
done

for worker in beta gamma; do
	# A worker needs an invitation only the first time it joins.
	join=()
	if [ ! -f "$POOL_DIR/$worker/known.json" ]; then
		join=(--join "$(alpha pool invite)")
	fi
	"$BIN" run --role worker --coordinator "$ADDR" "${join[@]}" --data-dir "$POOL_DIR/$worker" \
		--name "$worker" --slots 4 "${kubo[@]:0:1}" >"$POOL_DIR/$worker.log" 2>&1 &
	pids+=($!)
done
for _ in $(seq 300); do
	[ "$(alpha nodes 2>/dev/null | grep -c primes)" = 2 ] && break
	sleep 0.1
done

alpha nodes
cat <<TEXT

The pool is running. In another terminal:

  export ADDR=$ADDR DATA_DIR=$POOL_DIR/alpha
  examples/wordcount.sh
  examples/primes.sh
  examples/wordcount.sh "\$(examples/big-file.sh 50)"

or drive it directly:

  bin/sisyphusd nodes --addr \$ADDR --data-dir \$DATA_DIR
  bin/sisyphusd blob pins --addr \$ADDR --data-dir \$DATA_DIR

Logs are in $POOL_DIR/*.log. Ctrl-C here stops the pool.
TEXT
wait
