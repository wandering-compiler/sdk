package sqlitecollate

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Fingerprint identifies the text ORDERING AND CASE FOLDING this build
// applies, as a short opaque string.
//
// T1-4 pass #12, B-F5. This package's doc says registering the collation
// from one shared package "guarantees the ordering an index was built
// with is byte-for-byte the ordering a query compares with". That
// guarantee holds within one BUILD and not across builds, which is the
// only arrangement that exists in practice: the migration applier, the
// dev-DB snapshotter (its `VACUUM INTO` rebuilds every index) and each
// generated runtime are separate binaries, compiled at different times
// from different go.sums. `x/text` is a direct dependency in one and
// indirect in another, and the `upper`/`lower` UDFs use the Go
// toolchain's Unicode tables, which move independently of x/text
// entirely. When those tables disagree, an index is sorted one way and
// the queries reading it compare another — silently.
//
// The fingerprint is BEHAVIOUR-anchored, not a module version. Version
// strings answer a proxy question ("were these built from the same
// source?") and go stale in both directions: a patch release that does
// not touch the tables looks like drift, and a vendored or patched build
// that does looks identical. Sorting a probe set and hashing the result
// answers the question actually being asked. The two Unicode version
// strings ride along because a mismatch has to be diagnosable, not just
// detectable — they say WHICH tables differ.
//
// The probe deliberately spans the axes the emitted SQL depends on:
// accents (the as_cs contract), case (the same), non-Latin scripts,
// combining marks in both normal forms (NFC/NFD order is the axis B-F4
// measured), and characters whose upper/lower mappings are famously
// locale- and version-sensitive.
func Fingerprint() string {
	probe := []string{
		"a", "A", "á", "Á", "ä", "Ä", "z", "Z",
		"café", "café", // NFC vs NFD of the same word
		"Straße", "STRASSE",
		"ı", "I", "i", "İ", // dotted/dotless i
		"ǅ", "Ǆ", "ǆ", // titlecase-bearing digraph
		"日本", "にほん", "ﾆﾎﾝ",
		"Ω", "ω", "Ω", // greek capital omega vs ohm sign
		"é", "é",
	}

	ordered := make([]string, len(probe))
	copy(ordered, probe)
	sort.SliceStable(ordered, func(i, j int) bool {
		return Compare(ordered[i], ordered[j]) < 0
	})

	h := sha256.New()
	// Version strings first, so the hash changes when they do even if the
	// probe happens to sort identically.
	h.Write([]byte("unicode=" + unicode.Version + "\n"))
	h.Write([]byte("xtext=" + norm.Version + "\n"))
	for _, s := range ordered {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	// The case-folding UDFs are a SECOND producer of ordering-relevant
	// behaviour (expression indexes over upper()/lower() are advertised),
	// and they run on the Go toolchain's tables rather than x/text's — so
	// they get their own contribution.
	for _, s := range probe {
		h.Write([]byte(strings.ToUpper(s)))
		h.Write([]byte{0})
		h.Write([]byte(strings.ToLower(s)))
		h.Write([]byte{0})
	}

	return "w17c1:" + unicode.Version + ":" + norm.Version + ":" +
		hex.EncodeToString(h.Sum(nil))[:16]
}
