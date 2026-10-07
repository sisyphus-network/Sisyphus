#!/usr/bin/env bash
# Runs a small network on this machine for the desktop app to be built
# against, and leaves it running. Stop it with Ctrl-C.
#
#   examples/desktop-backend.sh
#
# What comes up:
#
#   alpha   the node the desktop app talks to. It runs a pool and a worker,
#           and serves the local API on 127.0.0.1:50051.
#   beta    a worker of alpha's pool, joined by invitation.
#   gamma   a node with a pool of its own that alpha has never met. The two
#           find each other, so gamma shows up in alpha's peer list as a
#           node to trust or not.
#
# Their files go in a directory of their own (POOL_DIR, default
# ./sisyphus-desktop-pool) and are kept between runs.
set -euo pipefail
cd "$(dirname "$0")/.."

POOL_DIR=${POOL_DIR:-$PWD/sisyphus-desktop-pool}
API=${SISYPHUS_API_ADDRESS:-127.0.0.1:50051}
ALPHA=127.0.0.1:${ALPHA_PORT:-7700}
GAMMA_PORT=${GAMMA_PORT:-7710}
mkdir -p "$POOL_DIR"

go build -o bin/sisyphusd ./apps/sisyphusd
BIN=bin/sisyphusd

pids=()
trap 'echo; echo "stopping the nodes"; kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true' EXIT

# Nodes that are to find each other listen where the others can reach them,
# not on this machine's loopback alone.
"$BIN" run --listen "0.0.0.0:${ALPHA#*:}" --data-dir "$POOL_DIR/alpha" --name alpha --slots 2 \
	--api-listen "$API" >"$POOL_DIR/alpha.log" 2>&1 &
pids+=($!)
alpha() { "$BIN" "$@" --addr "$ALPHA" --data-dir "$POOL_DIR/alpha"; }
for _ in $(seq 300); do
	alpha nodes >/dev/null 2>&1 && break
	sleep 0.1
done

join=()
if [ ! -f "$POOL_DIR/beta/known.json" ]; then
	join=(--join "$(alpha pool invite)")
fi
"$BIN" run --role worker --coordinator "$ALPHA" "${join[@]}" --data-dir "$POOL_DIR/beta" --name beta --slots 2 \
	>"$POOL_DIR/beta.log" 2>&1 &
pids+=($!)

"$BIN" run --listen "0.0.0.0:$GAMMA_PORT" --data-dir "$POOL_DIR/gamma" --name gamma --slots 2 \
	>"$POOL_DIR/gamma.log" 2>&1 &
pids+=($!)

for _ in $(seq 300); do
	[ "$(alpha nodes 2>/dev/null | grep -c primes)" = 2 ] && break
	sleep 0.1
done
# Something for the jobs view to show from the start.
alpha job submit --tasks 4 --params '{"from":0,"to":2000000}' >/dev/null

alpha nodes
cat <<TEXT

The backend is running. Start the desktop app against it, in another terminal:

  cd apps/sisyphus
  SISYPHUS_API_ADDRESS=$API SISYPHUS_API_TOKEN_FILE=$POOL_DIR/alpha/api.token npm run dev

Or ask it the questions the app asks, without opening a window:

  cd apps/sisyphus && node dev/check-daemon.mjs $API $POOL_DIR/alpha/api.token

gamma's node ID, to look for in the peer list:  $("$BIN" id --data-dir "$POOL_DIR/gamma")

More jobs, from here:

  bin/sisyphusd job submit --addr $ALPHA --data-dir $POOL_DIR/alpha --params '{"from":0,"to":50000000}'

Logs are in $POOL_DIR/*.log. Ctrl-C here stops the nodes.
TEXT
wait
