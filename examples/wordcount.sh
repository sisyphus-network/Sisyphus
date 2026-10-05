#!/usr/bin/env bash
# Counts the words in a file, on the pool.
#
#   examples/wordcount.sh [file] [how-many-words-to-show]
#
# Stores the file on the node, runs a wordcount job split across the pool's
# workers, and prints the most frequent words from the result.
source "$(dirname "$0")/common.sh"

file=${1:-$HERE/boulder.txt}
top=${2:-10}

echo "storing $file"
input=$(node blob put "$file")
echo "  as $input"

echo "counting words"
out=$(node job submit --workload wordcount --params "{\"input\":\"$input\"}")
echo "$out" | sed -n 's/^job \([0-9a-f]*\) succeeded in \(.*\)/  job \1 took \2/p'
result=$(echo "$out" | tail -n 1)
output=$(echo "$result" | sed 's/.*"output":"\([^"]*\)".*/\1/')
echo "  $result"

echo "the $top most frequent words, from the result $output"
node blob get "$output" | head -n "$top" | sed 's/^/  /'
