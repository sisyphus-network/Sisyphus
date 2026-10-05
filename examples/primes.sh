#!/usr/bin/env bash
# Counts the primes below a number twice, to show what splitting a job buys:
# once split across every worker slot in the pool, once whole on one worker.
#
#   examples/primes.sh [upper-bound]
source "$(dirname "$0")/common.sh"

to=${1:-2000000000}
params="{\"from\":0,\"to\":$to}"

# report <label> <job output>: the time taken and the answer.
report() {
	local took count
	took=$(echo "$2" | sed -n 's/^job [0-9a-f]* succeeded in \(.*\)/\1/p')
	count=$(echo "$2" | tail -n 1)
	printf '  %-22s %-16s %s\n' "$1" "$took" "$count"
}

echo "counting the primes below $to"
split=$(node job submit --params "$params")
tasks=$(echo "$split" | sed -n 's/^job [0-9a-f]*: \([0-9]*\) task(s)/\1/p')
report "split into $tasks tasks" "$split"
report "as a single task" "$(node job submit --mode full-worker --params "$params")"
