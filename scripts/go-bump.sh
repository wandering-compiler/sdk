#!/usr/bin/env bash
# go-bump.sh <line> — move the repository to another Go release line (e.g.
# 1.28): go.version, goversion.Minor, and every image, go.mod / go.work line
# and setup-go version check-go-version.sh checks. The pinned image digest
# (goversion.AlpineImagePinned) is printed, not guessed: set it from
#   docker buildx imagetools inspect golang:<line>-alpine
# A patch release of the same line changes nothing here but that digest.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
new=${1:?usage: scripts/go-bump.sh <line, e.g. 1.28>}
old=$(tr -d '[:space:]' < go.version)
[ "$new" != "$old" ] || { echo "already on Go $new"; exit 0; }
o=${old//./\\.}
scripts/go-version-files.sh | while IFS= read -r f; do
	perl -pi -e "s/golang:${o}(?![0-9.])/golang:${new}/g; s/^go ${o}(\\.[0-9]+)?\\s*\$/go ${new}.0\\n/; s/(go-version: .)${o}\\.x/\${1}${new}.x/g" "$f"
done
printf '%s\n' "$new" > go.version
# The SDK's goversion package carries the line into the compiler and w17ctl.
if [ -f go/tooling/goversion/goversion.go ]; then
	perl -pi -e "s/^const Minor = \"${o}\"/const Minor = \"${new}\"/" go/tooling/goversion/goversion.go
fi
echo "moved Go $old -> $new. Now: in the SDK set goversion.AlpineImagePinned's digest; regenerate; scripts/check-go-version.sh"
