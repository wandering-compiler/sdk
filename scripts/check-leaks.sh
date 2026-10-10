#!/usr/bin/env bash
# check-leaks.sh [path...] — nothing internal in a public repository.
#
# This repository is public. A comment, test or example carries the technical
# reason for what it says; it never carries where that reason came from inside
# the project that develops it: a person's or an adopter's name, a private
# host, a ticket or audit reference, a pull-request number of a private
# repository, a path into a private repository, or a dated anecdote. Each
# pattern below is one of those shapes.
#
# Paths default to the whole repository. Exit 1 lists every hit.
set -euo pipefail
cd "$(dirname "$0")/.."
[ $# -gt 0 ] || set -- .

# name|ERE — case-insensitive.
patterns=(
	"a person's name|\bjiri\b|dubansky|s1lent"
	"an adopter's name|\bmarb\b|marb-ai|marb\.ai|\brehab\b|\bdeinvo\b"
	"a private host or path|178\.215\.|/home/workspaces/|\bw17-0[0-9]\b|forge-(cleanup|ports)"
	"a ticket reference|\bREV-[0-9]+"
	"an audit reference|\bT[0-9]-[0-9]+\b|\bpass #[0-9]+|\b[A-D]-F[0-9]+\b|\b[A-D][0-9]+-[0-9]+\b"
	"a pull-request number|(^|[^&0-9a-z#/])#[0-9]{2,4}\b"
	"a path into a private repository|docs/(decisions|todos|specs|archive|experiments)/|\bsrcgo/|claude\.md|console-app/"
	"a dated anecdote|\(a consumer\b[^)]*\)|a consumer, 20[0-9]{2}"
)

fail=0
for entry in "${patterns[@]}"; do
	name="${entry%%|*}"
	re="${entry#*|}"
	if hits="$(git grep -n -I -i -E "$re" -- "$@" ':!scripts/check-leaks.sh' ':!**/go.sum' 2>/dev/null)"; then
		echo "── $name: $(printf '%s\n' "$hits" | wc -l)"
		printf '%s\n' "$hits" | sed 's/^/   /'
		fail=1
	fi
done
# File names carry the same references (a test named after the audit pass that
# found it).
if names="$(git ls-files -- "$@" | grep -i -E '(^|[/_.-])(pass|rev|t[0-9]-?[0-9])[0-9_-]*[0-9]([/_.-]|$)')"; then
	echo "── a reference in a file name: $(printf '%s\n' "$names" | wc -l)"
	printf '%s\n' "$names" | sed 's/^/   /'
	fail=1
fi
if [ "$fail" = 1 ]; then
	echo "check-leaks: internal references found (see above) — say the technical reason instead"
	exit 1
fi
echo "check-leaks ok"
