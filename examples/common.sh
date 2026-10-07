# Shared by the example scripts. Source it; do not run it.
#
# The examples talk to a node you are allowed to use:
#   ADDR      its address                  (default 127.0.0.1:7700)
#   DATA_DIR  where your key for it lives  (default ~/.sisyphus)
#
# On the machine a node runs on, the defaults make you its owner. From
# another machine, join the node first; see the README in this directory.

set -euo pipefail

ADDR=${ADDR:-127.0.0.1:7700}
DATA_DIR=${DATA_DIR:-$HOME/.sisyphus}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Use a sisyphusd on the PATH, or the one built in this repository.
if command -v sisyphusd >/dev/null 2>&1; then
	SISYPHUSD=sisyphusd
elif [ -x "$HERE/../bin/sisyphusd" ]; then
	SISYPHUSD=$HERE/../bin/sisyphusd
else
	echo "sisyphusd not found: run 'make build' in the repository, or put sisyphusd on your PATH" >&2
	exit 1
fi

# node <command...> runs a sisyphusd command against the node.
node() {
	local command=$1
	shift
	case $command in
	job | blob | pool)
		local sub=$1
		shift
		"$SISYPHUSD" "$command" "$sub" --addr "$ADDR" --data-dir "$DATA_DIR" "$@"
		;;
	*)
		"$SISYPHUSD" "$command" --addr "$ADDR" --data-dir "$DATA_DIR" "$@"
		;;
	esac
}

if ! node nodes >/dev/null 2>&1; then
	echo "cannot reach a node at $ADDR as the key in $DATA_DIR" >&2
	echo "start one with 'examples/local-pool.sh', or set ADDR and DATA_DIR" >&2
	exit 1
fi
