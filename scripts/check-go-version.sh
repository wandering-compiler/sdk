#!/usr/bin/env bash
# check-go-version.sh — every Go image, go.mod / go.work `go` line and
# setup-go version in the repository names the line in go.version:
#   golang:<line>[-alpine…]   the image floats to the line's newest patch
#   go <line>.0               a go.mod states the line's first release, a minimum
#   go-version: '<line>.x'    setup-go takes the line's newest patch
# A Go upgrade is `scripts/go-bump.sh <line>`; this is what catches the place
# it did not reach.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
line=$(tr -d '[:space:]' < go.version)
bad=0
while IFS= read -r f; do
	[ -n "$f" ] || continue
	while IFS= read -r hit; do
		echo "$f:$hit"
		bad=1
	done < <(grep -nIE 'golang:1\.[0-9]+|^go 1\.[0-9]+|go-version: .1\.[0-9]+' "$f" |
		grep -vE "golang:${line//./\\.}([^0-9.]|$)|^[0-9]+:go ${line//./\\.}\.0\s*$|go-version: .${line//./\\.}\.x." || true)
done < <(scripts/go-version-files.sh)
if [ "$bad" -ne 0 ]; then
	echo "check-go-version: the lines above do not name Go $line (go.version) — run scripts/go-bump.sh $line, or fix them by hand" >&2
	exit 1
fi
echo "check-go-version: every Go image, go line and setup-go version names $line"
