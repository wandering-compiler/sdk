// Package goversion is the Go release line w17 builds with and generates for:
// the one place a Go upgrade changes in code. The repository's go.version
// holds the same line for the Makefile and the scripts, and
// scripts/check-go-version.sh fails CI when a go.mod, Dockerfile or script
// drifts from it. The SDK carries it so the compiler and w17ctl read one value.
//
// Only the line is pinned. A go.mod states the line's first release — a
// minimum, so any patch of the line builds it without fetching another
// toolchain — and an image floats to the line's newest patch, so a Go
// security release reaches every build without a change here. The one
// exception is a digest-pinned image, which a patch release moves.
package goversion

// Minor is the Go release line.
const Minor = "1.27"

// Directive is the `go` line of every go.mod and go.work w17 writes.
const Directive = Minor + ".0"

// Image is the Go toolchain image (Debian), newest patch of the line.
const Image = "golang:" + Minor

// AlpineTag is AlpineImage without the repository, for options that take a tag.
const AlpineTag = Minor + "-alpine"

// AlpineImage is the build stage of every generated Go Dockerfile.
const AlpineImage = "golang:" + Minor + "-alpine"

// AlpineImagePinned is AlpineImage pinned to its multi-platform index digest,
// for builds that must not change under a commit (the swarm stack's image
// builds). A Go patch release moves it.
const AlpineImagePinned = AlpineImage + "@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280"
