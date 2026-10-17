#!/usr/bin/env bash
set -euo pipefail

attempts=${SISYPHUS_CI_DOWNLOAD_ATTEMPTS:-4}
retry_delay=${SISYPHUS_CI_DOWNLOAD_RETRY_DELAY_SECONDS:-5}
if [[ ! $attempts =~ ^[1-9][0-9]*$ || ! $retry_delay =~ ^[0-9]+$ ]]; then
	echo "invalid release-download retry settings" >&2
	exit 2
fi

retryable='error connecting to api\.github\.com|HTTP (429|5[0-9]{2})|rate limit exceeded|connection (reset|refused)|network is unreachable|temporary failure|no such host|TLS handshake timeout|i/o timeout|context deadline exceeded|unexpected EOF'

for ((attempt = 1; attempt <= attempts; attempt++)); do
	if output=$(gh release download --clobber "$@" 2>&1); then
		[[ -z $output ]] || printf '%s\n' "$output"
		exit 0
	else
		status=$?
	fi
	printf '%s\n' "$output" >&2
	if ((attempt == attempts)) || ! grep -Eiq "$retryable" <<<"$output"; then
		exit "$status"
	fi
	delay=$((attempt * retry_delay))
	echo "transient release download failure; retrying in ${delay}s (attempt ${attempt}/${attempts})" >&2
	sleep "$delay"
done
