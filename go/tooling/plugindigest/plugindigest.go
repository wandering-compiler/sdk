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
	// A throwaway project `w17ctl plugin dev` built to run this plugin as
	// itself.
	//
	// ⚠️ THE SAME CIRCULARITY plugin.sig has, one level up. `plugin dev --out`
	// can put its project inside the tree it is testing, and that project is
	// BUILT BY installing this plugin — which digests it. So the digest would
	// change while the thing being digested was being generated, and no two
	// runs would agree.
	//
	// A directory name rather than a root-relative path, unlike plugin.sig: a
	// dev project is a dev project wherever it sits, and there is no legitimate
	// `.w17dev` inside a plugin that a consumer compiles.
	//
	// The published form does not carry it either — the render copies
	// plugin.yaml, README, proto/ and src/, and this is none of those — so this
	// is the second of two places that have to agree, not the only one.
	".w17dev": true,
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

// skipRootFiles are dropped by their path RELATIVE to the plugin root, so the
// exclusion covers exactly the one file that has to be excluded and nothing
// that merely shares its name.
//
// `plugin.sig` is the signature file, which cannot be part of what it signs. A
// plugin's signature binds this digest; if the digest covered the file holding
// it, writing the signature would change the digest it was computed over, and
// no signed plugin could ever verify — a circularity with no fixed point, not a
// bug that shows up sometimes.
//
// The exclusion lives here rather than in the signer because BOTH ends have to
// agree: the publisher hashes a tree with no signature in it yet, and the
// consumer hashes one that has had a signature added. Two definitions of the
// digest is not a check.
//
// ⚠️ ROOT-relative, not by name like `skipFiles` above. The signature is read
// from the root and nowhere else, so an `internal/plugin.sig` is an ordinary
// file the author wrote — and excluding it by name would carve a hole in the
// digest at every depth of the tree, which is somewhere to put content that a
// signed plugin does not cover.
var skipRootFiles = map[string]bool{
	"plugin.sig": true,
}

// Check reports whether s has the shape `Of` produces: 64 lowercase hex
// characters.
//
// It lives beside `Of` because the shape is this package's to define, and both
// ends of a signature need the same answer — the publisher, which must not mint
// a claim over something no tree can yield, and anyone reading a digest off the
// wire.
//
// ⚠️ LOWERCASE IS REQUIRED, NOT NORMALISED. `Of` emits lowercase, so uppercase
// hex means the caller computed the digest some other way — and a caller who
// did that is a caller whose digest may differ from `Of`'s in ways case-folding
// would hide. Refusing says so; quietly lowercasing would let the real
// disagreement through.
func Check(s string) error {
	const want = sha256.Size * 2
	if len(s) != want {
		return fmt.Errorf("plugin digest: %d characters, want %d — this is not a plugin digest", len(s), want)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return fmt.Errorf("plugin digest: %q at position %d — a plugin digest is lowercase hex", string(c), i)
	}
	return nil
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
		// After the regular-file check on purpose: a symlinked `plugin.sig`
		// is still refused, because "a plugin tree carries files" is a
		// statement about the tree and not about what the digest covers.
		if skipRootFiles[filepath.ToSlash(rel)] {
			return nil
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
