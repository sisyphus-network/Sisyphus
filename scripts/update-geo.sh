#!/usr/bin/env bash
# Remakes the table of countries the daemon carries, packages/geo/countries.bin.gz,
# from what the five regional Internet registries publish today.
#
#   scripts/update-geo.sh
set -euo pipefail
cd "$(dirname "$0")/.."

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for source in \
	ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest \
	ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-extended-latest \
	ftp.apnic.net/stats/apnic/delegated-apnic-extended-latest \
	ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-extended-latest \
	ftp.afrinic.net/pub/stats/afrinic/delegated-afrinic-extended-latest; do
	echo "fetching $source"
	curl --fail --silent --show-error --max-time 300 -o "$tmp/$(basename "$source")" "https://$source"
done

cd packages/geo
go run gen.go "$tmp"/delegated-*
date -u +%Y-%m-%d > countries.date
