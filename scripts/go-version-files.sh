#!/usr/bin/env bash
# go-version-files.sh — the tracked files that name the Go release line, for
# check-go-version.sh and go-bump.sh. A test's go.mod fixture models any
# project, so it is not listed, and a plugin vendored into a project
# (proto/plugins/) is its release's, moved by installing a newer one.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
git ls-files -z -- . \
	':!**/proto/plugins/**' ':!*_test.go' ':!**/testdata/**' ':!*.pb.go' ':!**/go.sum' ':!**/Cargo.lock' \
	':!*.png' ':!*.jpg' ':!*.ico' ':!*.woff*' |
	xargs -0 grep -lIE 'golang:1\.[0-9]+|^go 1\.[0-9]+|go-version: .1\.[0-9]+' -- 2>/dev/null || true
