#!/usr/bin/env bash
# Counts the words in a file without the pool's storage ever holding the
# file, or the result, in a form anyone but you and the job's workers can
# read.
#
#   examples/private-wordcount.sh [file] [how-many-words-to-show]
#
# The file is sealed with a key on this machine before it is stored. The
# job is given the key, so its workers can read the input and seal what they
# produce. Other members of the pool can fetch all of it and read none.
#
# The key is kept in sisyphus-job.key beside where you run this (KEY_FILE to
# change), made on first use. Whoever has that file can read the data.
source "$(dirname "$0")/common.sh"

file=${1:-$HERE/boulder.txt}
top=${2:-10}
key=${KEY_FILE:-sisyphus-job.key}

if [ ! -f "$key" ]; then
	"$SISYPHUSD" key new "$key"
	echo "made a key: $key"
fi

echo "sealing and storing $file"
input=$(node blob put --key-file "$key" "$file")
echo "  as $input"

echo "counting words, privately"
out=$(node job submit --workload wordcount --key-file "$key" --params "{\"input\":\"$input\"}")
echo "$out" | sed -n 's/^job \([0-9a-f]*\) succeeded in \(.*\)/  job \1 took \2/p'
result=$(echo "$out" | tail -n 1)
output=$(echo "$result" | sed 's/.*"output":"\([^"]*\)".*/\1/')
echo "  $result"

echo "what the pool holds for the result, as anyone in it can fetch it:"
node blob get "$output" | head -c 48 | od -An -c | head -n 2 | sed 's/^/  /'

echo "the $top most frequent words, unsealed with the key:"
node blob get --key-file "$key" "$output" | head -n "$top" | sed 's/^/  /'
