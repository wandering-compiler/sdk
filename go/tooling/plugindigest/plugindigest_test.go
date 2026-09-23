package plugindigest

import (
	"os"
	"path/filepath"
	"testing"
)

// write materialises a tree from a path→content map, creating parents.
func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func digestOf(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, files)
	d, err := Of(dir)
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	return d
}

// The whole point: the same bytes must hash the same, every time and on every
// machine, or the digest cannot be a pin.
func TestOf_IsDeterministic(t *testing.T) {
	files := map[string]string{
		"plugin.yaml":            "name: auth\nversion: 0.1.0-rc.1\n",
		"proto/auth_query.proto": "syntax = \"proto3\";\n",
		"src/handlers/signin.go": "package handlers\n",
	}
	a, b := digestOf(t, files), digestOf(t, files)
	if a != b {
		t.Fatalf("two digests of identical trees differ:\n  %s\n  %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("digest is not a hex sha256: %q", a)
	}
}

// Content is covered, which is the obvious half.
func TestOf_ContentChangeChangesTheDigest(t *testing.T) {
	a := digestOf(t, map[string]string{"src/x.go": "package a\n"})
	b := digestOf(t, map[string]string{"src/x.go": "package b\n"})
	if a == b {
		t.Fatal("a changed file did not change the digest — the pin would accept tampered code")
	}
}

// Paths are covered too: moving the same bytes to another file is a different
// tree, and a digest that says otherwise lets a plugin be restructured under a
// pin that claims it did not change.
func TestOf_PathChangeChangesTheDigest(t *testing.T) {
	a := digestOf(t, map[string]string{"src/x.go": "package a\n"})
	b := digestOf(t, map[string]string{"src/y.go": "package a\n"})
	if a == b {
		t.Fatal("moving content to a different path did not change the digest")
	}
}

// The injectivity case the migrate.ContentHash comment warns about, applied to
// a tree: without length-prefixing, "ab" + "c" and "a" + "bc" feed the hash the
// same byte stream, so two different trees collide. Both halves are varied —
// path and content — because the boundary between them is exactly what a naive
// concatenation loses.
func TestOf_ShiftedBoundariesDoNotCollide(t *testing.T) {
	// The pair is constructed, not guessed. Both trees hold TWO non-executable
	// files, so the count and the mode bytes are identical; the only difference
	// is where one file's content stops and the next file's path starts:
	//
	//   {"a": "bc", "d": "e"}  →  a  -  bc  d  -  e
	//   {"a": "b", "cd": "e"}  →  a  -  b   cd -  e
	//
	// Unprefixed, both are the byte stream `a-bcd-e`. An earlier version of
	// this test used {"ab":"c"} vs {"a":"bc"} and passed even with the prefixes
	// removed — the mode byte between path and content happened to separate
	// that pair, so the test was green for a reason that had nothing to do with
	// the property it claimed to check.
	a := digestOf(t, map[string]string{"a": "bc", "d": "e"})
	b := digestOf(t, map[string]string{"a": "b", "cd": "e"})
	if a == b {
		t.Fatal("two trees that differ only in where the path ends and the content begins " +
			"collide — the encoding is not injective")
	}
}

// An empty file is a fact about the tree, not the absence of one.
func TestOf_EmptyFileIsNotNothing(t *testing.T) {
	a := digestOf(t, map[string]string{"a": "x", "b": ""})
	b := digestOf(t, map[string]string{"a": "x"})
	if a == b {
		t.Fatal("a tree with an extra EMPTY file hashes the same as one without it")
	}
}

// The executable bit is the difference between a script that runs and one that
// does not, so it is part of what the pin covers.
func TestOf_ExecutableBitIsCovered(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"run.sh": "#!/bin/sh\necho hi\n"})
	before, err := Of(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	after, err := Of(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("making a file executable did not change the digest")
	}
}

// A fetched tree arrives with the clone's `.git` in it. Hashing that would make
// the digest depend on when and how it was cloned, so two honest fetches of the
// same tag would disagree — the digest has to describe the PLUGIN.
func TestOf_IgnoresVCSMetadata(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"plugin.yaml":   "name: auth\n",
		".git/HEAD":     "ref: refs/heads/main\n",
		".git/config":   "[core]\n",
		".DS_Store":     "junk",
		"src/.gitkeep":  "",
		"sub/.git/HEAD": "ref: x\n",
	})
	a, err := Of(dir)
	if err != nil {
		t.Fatal(err)
	}

	clean := t.TempDir()
	write(t, clean, map[string]string{
		"plugin.yaml":  "name: auth\n",
		"src/.gitkeep": "",
	})
	b, err := Of(clean)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("VCS metadata leaked into the digest — two honest fetches of one tag would "+
			"disagree:\n  with .git: %s\n  without:   %s", a, b)
	}
}

// Directories carry no content of their own; only the files in them do. An
// empty directory cannot survive a git fetch anyway, so treating it as
// significant would make the digest depend on something the transport drops.
func TestOf_EmptyDirectoryIsNotPartOfTheTree(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"a": "x"})
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := Of(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := digestOf(t, map[string]string{"a": "x"})
	if a != b {
		t.Fatal("an empty directory changed the digest, but git cannot carry one")
	}
}

// A missing root is a caller error worth naming, not a digest of nothing.
func TestOf_MissingRootIsAnError(t *testing.T) {
	if _, err := Of(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("digesting a nonexistent tree returned no error — a typo would silently pin " +
			"the digest of an empty tree")
	}
}

// A plugin's standalone pb is regenerated — absent right after an install,
// present after a codegen run. Covering it would make the first codegen turn
// every pin into a mismatch, so the digest describes what a person writes.
func TestOf_IgnoresRegeneratedPb(t *testing.T) {
	base := map[string]string{
		"plugin.yaml":       "name: auth\n",
		"proto/a.proto":     "syntax = \"proto3\";\n",
		"src/handlers/x.go": "package handlers\n",
	}
	before := digestOf(t, base)

	withGen := map[string]string{}
	for k, v := range base {
		withGen[k] = v
	}
	withGen["src/gen/pb/a.pb.go"] = "// generated\npackage pb\n"
	withGen["src/gen/features.go"] = "package gen\n"

	if after := digestOf(t, withGen); after != before {
		t.Fatal("regenerating the plugin's pb changed the digest — every pin would mismatch " +
			"after the first codegen")
	}
}

// The control: everything OUTSIDE src/gen still counts, so the exclusion is a
// hole for generated output and not a hole in the check.
func TestOf_StillCoversHandwrittenSource(t *testing.T) {
	a := digestOf(t, map[string]string{"src/handlers/x.go": "package handlers\n"})
	b := digestOf(t, map[string]string{"src/handlers/x.go": "package handlers\n\nfunc sneaky() {}\n"})
	if a == b {
		t.Fatal("an edit to a handler did not change the digest")
	}
}
