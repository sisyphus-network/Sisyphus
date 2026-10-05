#!/usr/bin/env bash
# Runs the local pool in tmux, with a second pane beside it already set up to
# use it, so that everything is in one terminal.
#
#   examples/tmux-pool.sh [--kubo] [--detach]
#
# Left pane: the pool (examples/local-pool.sh), with its node list and logs.
# Right pane: a shell with ADDR and DATA_DIR set, ready for
# examples/wordcount.sh and the rest.
#
# Leave with Ctrl-b d, which keeps it all running in the background (come
# back with: tmux attach -t sisyphus), or end it with
#   tmux kill-session -t sisyphus
# which also stops the pool. --detach starts it without attaching.
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v tmux >/dev/null 2>&1; then
	echo "tmux is not installed; run examples/local-pool.sh in one terminal and use another for commands" >&2
	exit 1
fi

pool_args=()
attach=1
for arg in "$@"; do
	case $arg in
	--detach) attach=0 ;;
	*) pool_args+=("$arg") ;;
	esac
done

SESSION=${SESSION:-sisyphus}
export ADDR=${ADDR:-127.0.0.1:7700}
export POOL_DIR=${POOL_DIR:-$PWD/sisyphus-pool}

if tmux has-session -t "$SESSION" 2>/dev/null; then
	echo "a tmux session named $SESSION is already running: tmux attach -t $SESSION" >&2
	exit 1
fi

tmux new-session -d -s "$SESSION" -n pool "examples/local-pool.sh ${pool_args[*]:-}"
# The second pane is an ordinary shell that knows where the pool is.
tmux split-window -h -t "$SESSION" \
	"export ADDR='$ADDR' DATA_DIR='$POOL_DIR/alpha'; echo 'The pool is starting in the left pane. Try: examples/primes.sh'; exec \"\${SHELL:-bash}\""
tmux select-pane -t "$SESSION" -R

if [ "$attach" = 1 ]; then
	exec tmux attach -t "$SESSION"
fi
echo "started in the background: tmux attach -t $SESSION"
