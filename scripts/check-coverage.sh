#!/usr/bin/env bash
# Reads a Go coverage profile and fails if any statement was never executed,
# listing each one. A block can appear once per test binary, so it counts as
# covered if any of them ran it.
set -euo pipefail

awk '
NR > 1 {
	if (!($1 in hits) || $3 > hits[$1]) hits[$1] = $3
}
END {
	for (block in hits) {
		if (hits[block] == 0) {
			print "not covered: " block
			missing++
		}
	}
	if (missing) {
		print missing " block(s) of code are not executed by any test"
		exit 1
	}
	print "every statement is covered"
}' "$1"
