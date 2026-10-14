#!/bin/sh
# The admin runtime carries a copy of the browser half of the DPoP client
# (ts/dpop/src → ts/admin-runtime/src/dpop). A copy, not a dependency: the
# platform embeds ts/admin-runtime alone into w17ctl and a generated admin
# depends on it by path, so an @w17/dpop dependency would point at a package
# that is not there. ts/dpop is where the signer is authored and tested.
#
#   tools/admin-dpop.sh sync    copy the files over
#   tools/admin-dpop.sh check   fail when the copy differs from the source,
#                               or holds a file the source does not
#
# node.ts (the file key store) stays out: it imports node:fs, and the admin
# is compiled by a browser bundler.
set -eu

root=$(dirname "$0")/..
src=$root/ts/dpop/src
dst=$root/ts/admin-runtime/src/dpop
files="base64url.ts fetch.ts index.ts jwk.ts key.ts proof.ts ticket.ts"

case "${1:-}" in
sync)
	mkdir -p "$dst"
	for f in $files; do cp "$src/$f" "$dst/$f"; done
	echo "admin-dpop sync ok: $dst from $src"
	;;
check)
	for f in $files; do
		if ! cmp -s "$src/$f" "$dst/$f"; then
			echo "the admin runtime's copy of the DPoP client has drifted:"
			echo "  source $src/$f"
			echo "  copy   $dst/$f"
			echo "run 'tools/admin-dpop.sh sync' (ts/dpop is the source of truth)"
			exit 1
		fi
	done
	for p in "$dst"/*; do
		f=$(basename "$p")
		case " $files " in
		*" $f "*) ;;
		*)
			echo "$dst/$f is not one of the copied files ($files)"
			exit 1
			;;
		esac
	done
	echo "admin-dpop check ok"
	;;
*)
	echo "usage: $0 sync|check" >&2
	exit 2
	;;
esac
