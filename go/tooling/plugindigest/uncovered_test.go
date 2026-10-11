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
	})
	got, err := Uncovered(dir)
	if err != nil {
		t.Fatalf("Uncovered: %v", err)
	}
	want := []string{
		".w17dev/main.go",
		"src/gen/extra.go",
		"src/gen/pb/p.pb.go",
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
