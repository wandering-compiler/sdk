//go:build race

package redact

// The race detector makes sync.Pool drop items at random, so an
// allocation count means nothing under it.
const raceEnabled = true
