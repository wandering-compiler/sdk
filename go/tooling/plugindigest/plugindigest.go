// Package plugindigest computes the canonical content digest of a plugin
// tree — the value a lock pins beside the tag and the commit SHA, and the one
// both ends of the fetch compare.
//
// It exists in the public SDK because BOTH sides need the same answer: the
// thin client fetches a plugin from git and records what it got, and the
// console verifies that what it is asked to compile is what the lock pinned.
// A digest computed two different ways is not a check, so there is one
// implementation and both import it — the same reasoning that put
// migrate.ContentHash in one place.
//
// # Why a tag and a SHA are not enough
//
// A git tag is a movable pointer: re-point it and every consumer gets
// different bytes under a pin they already recorded. Pinning the commit SHA
// closes that, but only for a fetch that goes through git — and the tree
// reaches the compiler as FILES, after a clone, a sparse checkout, a copy into
// the project and (today) a commit into the consumer's repo. The digest is
// what survives all of that: it describes the bytes that will actually be
// compiled, not the route they took.
package plugindigest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// digestVersion tags the encoding. A future change to WHAT is covered (say,
// symlink targets) changes this, so an old digest can never silently compare
// equal to a new one computed over different facts.
const digestVersion = "w17-plugin-digest-v1"

// skipDirs are the directory names dropped wherever they appear.
//
// `.git` is the load-bearing one: a fetched tree arrives with the clone's
// metadata inside it, and that metadata differs between two honest fetches of
// the same tag (pack ordering, fetch depth, the remote's name). Hashing it
// would make the digest describe the CLONE rather than the plugin.
var skipDirs = map[string]bool{
	".git": true,
	// Reproduced by a consumer's own toolchain, never shipped, and large.
	"node_modules": true,
}

// skipPaths are dropped by their path RELATIVE to the plugin root.
//
// `src/gen` is the load-bearing one. A plugin's standalone pb is REGENERATED —
// by `w17ctl plugin gen-pb` for the author, and by codegen inside a consumer's
// project — so it is present after a codegen run and absent right after an
// install. A digest that covered it would describe the moment it was taken
// rather than the plugin, and the first codegen would turn every pin into a
// mismatch. What stays covered is everything a person writes: the manifest,
// the protos, the handlers.
var skipPaths = map[string]bool{
	"src/gen": true,
}

// skipFiles are dropped by exact name, anywhere in the tree. Editor and OS
// droppings only; anything a plugin author writes on purpose is covered.
var skipFiles = map[string]bool{
	".DS_Store": true,
}

// Of returns the hex sha256 digest of the plugin tree rooted at dir.
//
// The encoding is INJECTIVE, which is the only property that makes it a
// security check rather than a checksum: every field is length-prefixed, so no
// two distinct trees can feed the hash the same byte stream by shifting bytes
// across a boundary. Without it, {"ab": "c"} and {"a": "bc"} collide — see the
// same argument in migrate.ContentHash, which learned it the hard way.
//
// Covered: every regular file's path (slash-separated, relative to dir), its
// executable bit, and its content. Paths are sorted, so the walk order of the
// filesystem cannot change the answer.
//
// Not covered: directories (git cannot carry an empty one, so treating it as
// significant would make the digest depend on something the transport drops),
// and timestamps, ownership and the non-exec permission bits (none survive a
// clone intact).
func Of(dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("plugin digest: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("plugin digest: %s is not a directory", dir)
	}

	type entry struct {
		rel  string
		exec bool
	}
	var files []entry

	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != dir && skipDirs[name] {
				return filepath.SkipDir
			}
			if rel, rerr := filepath.Rel(dir, p); rerr == nil && skipPaths[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFiles[name] {
			return nil
		}
		// A symlink is neither its target nor its own content here. Refusing
		// is the safe side: a plugin tree that needs one is a design question,
		// and silently hashing the link text (or worse, following it out of
		// the tree) would answer it by accident.
		if !d.Type().IsRegular() {
			return fmt.Errorf("plugin digest: %s is not a regular file (%s) — "+
				"a plugin tree carries files, and anything else would make the digest depend on "+
				"what it points at", p, d.Type())
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		files = append(files, entry{rel: filepath.ToSlash(rel), exec: fi.Mode()&0o111 != 0})
		return nil
	})
	if err != nil {
		return "", err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	h := sha256.New()
	writeField(h, digestVersion)
	writeField(h, strconv.Itoa(len(files)))
	for _, f := range files {
		writeField(h, f.rel)
		if f.exec {
			writeField(h, "x")
		} else {
			writeField(h, "-")
		}
		body, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.rel)))
		if rerr != nil {
			return "", fmt.Errorf("plugin digest: %w", rerr)
		}
		writeField(h, string(body))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeField appends one length-prefixed field. The prefix is what makes the
// stream injective: a reader could recover the exact field boundaries from the
// bytes alone, so no two distinct field tuples produce the same stream.
func writeField(h io.Writer, s string) {
	var b strings.Builder
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
	_, _ = io.WriteString(h, b.String())
}
