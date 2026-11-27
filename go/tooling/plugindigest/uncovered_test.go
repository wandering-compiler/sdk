package plugindigest

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A file the digest does not cover, in a place a consumer compiles from, is
// unsigned code under a valid pin. Uncovered names every such file so a
// consumer can refuse the tree; clone metadata and the root signature are not
// among them.
func TestUncovered_NamesEveryFileOutsideTheDigest(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"plugin.yaml":                   "name: p\n",
		"plugin.sig":                    "sig",
		"src/handler.go.src":            "package p\n",
		"src/gen/pb/p.pb.go":            "package pb\n",
		"src/gen/extra.go":              "package gen // placed by hand\n",
		"web/node_modules/dep/index.js": "module.exports = 1\n",
		".w17dev/main.go":               "package main\n",
		"src/x/.w17dev/y.go":            "package y\n",
		".git/HEAD":                     "ref: refs/heads/main\n",
		"src/gen/.DS_Store":             "",
		"proto/.git/evil.proto":         "syntax = \"proto3\";\n",
		"src/handlers/.git/x.go":        "package x\n",
		"src/gen/.git/y.go":             "package y\n",
	})
	got, err := Uncovered(dir)
	if err != nil {
		t.Fatalf("Uncovered: %v", err)
	}
	want := []string{
		".w17dev/main.go",
		"proto/.git/evil.proto",
		"src/gen/.git/y.go",
		"src/gen/extra.go",
		"src/gen/pb/p.pb.go",
		"src/handlers/.git/x.go",
		"src/x/.w17dev/y.go",
		"web/node_modules/dep/index.js",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Uncovered = %v, want %v", got, want)
	}
}

// After Prune nothing in the tree is outside the digest, the digest itself is
// unchanged, and what the author wrote survives.
func TestPrune_LeavesOnlyCoveredContent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"plugin.yaml":                   "name: p\n",
		"src/handler.go.src":            "package p\n",
		"src/gen/extra.go":              "package gen\n",
		"web/node_modules/dep/index.js": "x\n",
		".w17dev/main.go":               "package main\n",
		".git/HEAD":                     "ref\n",
		"proto/.git/evil.proto":         "x\n",
	})
	before, err := Of(dir)
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if err := Prune(dir); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if left, err := Uncovered(dir); err != nil || len(left) != 0 {
		t.Fatalf("Uncovered after Prune = %v, %v; want none", left, err)
	}
	after, err := Of(dir)
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if before != after {
		t.Error("Prune changed the digest; it must only remove what the digest skips")
	}
	for _, keep := range []string{"plugin.yaml", "src/handler.go.src", ".git/HEAD"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(keep))); err != nil {
			t.Errorf("Prune removed %s: %v", keep, err)
		}
	}
}

// Prune removes a symlinked regenerated directory without following it.
func TestPrune_DoesNotFollowSymlinks(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, map[string]string{"keep.txt": "keep"})
	dir := t.TempDir()
	write(t, dir, map[string]string{"plugin.yaml": "name: p\n"})
	if err := os.Symlink(outside, filepath.Join(dir, "node_modules")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := Prune(dir); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatalf("Prune followed a symlink out of the tree: %v", err)
	}
}

// Symlinks and other non-regular files are refused, not hashed as text or
// followed out of the tree.
func TestOf_RefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"plugin.yaml": "name: p\n"})
	if err := os.Symlink("plugin.yaml", filepath.Join(dir, "link.yaml")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := Of(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Of = %v, want a refusal of the symlink", err)
	}
}

// Check accepts exactly 64 lower-case hex characters.
func TestCheck_RequiresLowerCaseHexOfTheRightLength(t *testing.T) {
	good := strings.Repeat("a1", 32)
	if err := Check(good); err != nil {
		t.Fatalf("Check(%q) = %v", good, err)
	}
	for _, bad := range []string{"", good[:63], good + "0", strings.ToUpper(good), strings.Repeat("g", 64)} {
		if err := Check(bad); err == nil {
			t.Errorf("Check(%q) accepted a malformed digest", bad)
		}
	}
}

// The per-path rule and the walk agree: for every file in a tree, Of moves
// when the file changes exactly when neither Uncovered nor Classify claims
// it. One rule with three readers is only one rule while they stay in step,
// and this is what keeps them there.
func TestClassify_AgreesWithOfAndUncovered(t *testing.T) {
	files := map[string]string{
		"plugin.yaml":                     "name: p\n",
		"proto/types/a.proto":             "syntax = \"proto3\";\n",
		"proto/node_modules/x.proto":      "x\n",
		"src/handlers/h.go":               "package h\n",
		"src/handlers/node_modules/y.go":  "package y\n",
		"src/gen/pb/p.pb.go":              "package pb\n",
		"src/gen/evil.go":                 "package gen\n",
		"src/general/ok.go":               "package general\n",
		"src/x/.w17dev/y.go":              "package y\n",
		".w17dev/proto/domains/d/w.proto": "w\n",
		"gen/top.go":                      "package gen\n",
		"node_modules.go":                 "package p\n",
	}
	dir := t.TempDir()
	write(t, dir, files)
	unc, err := Uncovered(dir)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, u := range unc {
		listed[u] = true
	}
	base, err := Of(dir)
	if err != nil {
		t.Fatal(err)
	}
	for rel := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		orig, _ := os.ReadFile(full)
		if err := os.WriteFile(full, append(orig, 'z'), 0o644); err != nil {
			t.Fatal(err)
		}
		moved, err := Of(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		covered := moved != base
		if covered == listed[rel] {
			t.Errorf("%s: covered by Of = %v, listed by Uncovered = %v — exactly one must hold", rel, covered, listed[rel])
		}
		if (Classify(rel) == NotCovered) != listed[rel] {
			t.Errorf("%s: Classify = %v, Uncovered lists it = %v", rel, Classify(rel), listed[rel])
		}
	}
	if !ToolchainWritten("src/gen/pb/p.pb.go") || ToolchainWritten("src/gen/evil.go") || ToolchainWritten("src/gen/pbx/a.go") {
		t.Error("ToolchainWritten does not carve out the pb")
	}
}

// Every location Of leaves out is classified, and the class is the walk's own
// answer: for each sample UncoveredSamples derives from the skip rules
// themselves, planting the file does not move the digest, Classify does not
// call it Covered, and Uncovered lists it exactly when Classify says
// NotCovered. A skip rule added to Of without a class shows up here as a
// sample Classify calls Covered — so no new skip can open a hole the readers
// do not see.
func TestClassify_EverySkipLocationIsClassified(t *testing.T) {
	samples := UncoveredSamples()
	if len(samples) == 0 {
		t.Fatal("UncoveredSamples is empty")
	}
	// The samples must reach every rule, or the walk below proves nothing.
	for _, must := range []string{".git/x.go", "proto/.git/x.proto", "src/gen/x.go", "node_modules/x.go",
		"src/handlers/.w17dev/x.go", ".DS_Store", "plugin.sig"} {
		found := false
		for _, s := range samples {
			found = found || s == must
		}
		if !found {
			t.Errorf("UncoveredSamples does not carry %s", must)
		}
	}
	base := map[string]string{"plugin.yaml": "name: p\n", "proto/a.proto": "syntax = \"proto3\";\n"}
	clean := digestOf(t, base)
	for _, rel := range samples {
		files := map[string]string{rel: "planted\n"}
		for k, v := range base {
			files[k] = v
		}
		dir := t.TempDir()
		write(t, dir, files)
		got, err := Of(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != clean {
			t.Errorf("%s: Of covers it, so it is not a skip location", rel)
		}
		c := Classify(rel)
		if c == Covered {
			t.Errorf("%s: Of leaves it out but Classify calls it Covered — a reader would ship it unchecked", rel)
		}
		unc, err := Uncovered(dir)
		if err != nil {
			t.Fatal(err)
		}
		listed := len(unc) == 1 && unc[0] == rel
		if listed != (c == NotCovered) {
			t.Errorf("%s: Classify = %v, Uncovered = %v", rel, c, unc)
		}
	}
}

// A nested `.git` is not clone metadata: git cannot commit one, so no release
// carries it, and a file in it is outside the digest. Only the plugin root's
// own `.git` is metadata.
func TestClassify_OnlyTheRootGitIsMetadata(t *testing.T) {
	for rel, want := range map[string]Class{
		".git/HEAD":               Metadata,
		".git/objects/ab/cd":      Metadata,
		".git":                    Covered, // a FILE named .git (a worktree's pointer) is hashed
		"proto/.git/evil.proto":   NotCovered,
		"src/handlers/.git/x.go":  NotCovered,
		"plugin.sig":              Metadata,
		"proto/plugin.sig":        Covered,
		"proto/.DS_Store":         Metadata,
		"src/gen/pb/p.pb.go":      NotCovered,
		"src/gen/features.go":     NotCovered,
		"src/handlers/a.go":       Covered,
		"src/general/a.go":        Covered,
		"proto/node_modules/x.go": NotCovered,
	} {
		if got := Classify(rel); got != want {
			t.Errorf("Classify(%s) = %v, want %v", rel, got, want)
		}
	}
}

// The toolchain-written carve-out is exactly the pb codegen writes back and the
// two files `w17ctl plugin gen` writes: every reader that refuses an uncovered
// file asks this one predicate.
func TestToolchainWritten_IsExactlyThePbAndTheTwoGeneratedFiles(t *testing.T) {
	for rel, want := range map[string]bool{
		"src/gen/pb/p.pb.go":       true,
		"src/gen/pb/a/b.pb.go":     true,
		"src/gen/interfaces.go":    true,
		"src/gen/features.go":      true,
		"src/gen/evil.go":          false,
		"src/gen/pbx/a.go":         false,
		"src/gen/sub/features.go":  false,
		"src/handlers/features.go": false,
		"proto/.git/x.proto":       false,
	} {
		if got := ToolchainWritten(rel); got != want {
			t.Errorf("ToolchainWritten(%s) = %v, want %v", rel, got, want)
		}
	}
}
