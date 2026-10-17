#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1-} != release || ${2-} != download || ${3-} != --clobber ||
	${4-} != v0.1.0 || ${5-} != -R || ${6-} != owner/repo ||
	${7-} != -p || ${8-} != asset.tar.gz ]]; then
	echo "release download arguments were not preserved" >&2
	exit 2
fi
count=$(cat "$FAKE_GH_COUNT_FILE" 2>/dev/null || echo 0)
count=$((count + 1))
printf '%s\n' "$count" >"$FAKE_GH_COUNT_FILE"
case $FAKE_GH_MODE in
	transient-then-success)
		if ((count < 3)); then
			echo "error connecting to api.github.com" >&2
			exit 1
		fi
		echo downloaded
		;;
	transient-always)
		echo "error connecting to api.github.com" >&2
		exit 1
		;;
	permanent)
		echo "release not found" >&2
		exit 22
		;;
	esac
FAKE_GH
chmod +x "$tmp/gh"
export PATH="$tmp:$PATH"
export SISYPHUS_CI_DOWNLOAD_RETRY_DELAY_SECONDS=0
export SISYPHUS_CI_DOWNLOAD_ATTEMPTS=4
export FAKE_GH_COUNT_FILE="$tmp/count"

reset_count() { : >"$FAKE_GH_COUNT_FILE"; }
count_calls() { cat "$FAKE_GH_COUNT_FILE"; }

reset_count
FAKE_GH_MODE=transient-then-success \
	bash "$script_dir/download-release-assets.sh" v0.1.0 -R owner/repo -p asset.tar.gz >/dev/null
[[ $(count_calls) == 3 ]]

reset_count
if FAKE_GH_MODE=permanent bash "$script_dir/download-release-assets.sh" v0.1.0 -R owner/repo -p asset.tar.gz >/dev/null 2>&1; then
	echo "permanent download error unexpectedly succeeded" >&2
	exit 1
fi
[[ $(count_calls) == 1 ]]

reset_count
if FAKE_GH_MODE=transient-always bash "$script_dir/download-release-assets.sh" v0.1.0 -R owner/repo -p asset.tar.gz >/dev/null 2>&1; then
	echo "transient download unexpectedly succeeded" >&2
	exit 1
fi
[[ $(count_calls) == 4 ]]

echo "release download retry tests passed"
