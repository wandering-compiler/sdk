// Package pathguard centralises the path-containment checks that keep a
// relative path supplied by an untrusted peer from escaping a trusted root
// (a staging tmpdir, the project root) via an absolute path or a `..`
// traversal. It is the SINGLE SOURCE OF TRUTH for these guards: the codegen
// daemon's input staging + output-dir validation and the thin client's
// write/delete apply boundary all route through it, so a future hardening fix
// (e.g. Windows volume names, symlink resolution) lands in one place instead
// of drifting across hand-copied variants.
//
// All checks are purely LEXICAL — they do not resolve symlinks. That is
// sufficient for the threat model (a buggy/compromised peer driving plain
// file writes/removes), because no guarded path is ever used to CREATE a
// symlink that could later bootstrap an escape.
package pathguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// dotDotPrefix is "../" in the host separator — the lexical signature of a
// path that climbs out of its base.
var dotDotPrefix = ".." + string(os.PathSeparator)

// hasControlChar reports whether s contains a control character (newline, CR,
// tab, NUL, …). A legitimate path segment never does; a filename that does is
// either malformed or an injection attempt — e.g. a proto filename with an
// embedded newline that would break out of an unquoted YAML scalar in a
// generated buf.gen.yaml. Rejecting it here (the shared staging guard) is
// defense-in-depth on top of quoting at each render site.
func hasControlChar(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// Join joins a relative path rel under base and returns the cleaned absolute
// destination, rejecting anything that would escape base: an empty or absolute
// rel, a `..` traversal (e.g. `../../etc/cron.d/x`), or a path that resolves to
// base ITSELF (`.`). The last matters because an op resolving to the root would
// let an os.RemoveAll wipe the whole tree. rel may use OS or forward slashes.
func Join(base, rel string) (string, error) {
	clean := filepath.FromSlash(rel)
	if clean == "" || filepath.IsAbs(clean) {
		return "", fmt.Errorf("path %q is empty or absolute", rel)
	}
	if hasControlChar(clean) {
		return "", fmt.Errorf("path %q contains a control character", rel)
	}
	dst := filepath.Join(base, clean)
	r, err := filepath.Rel(base, dst)
	if err != nil || r == "." || r == ".." || strings.HasPrefix(r, dotDotPrefix) {
		return "", fmt.Errorf("path %q escapes or targets the base dir", rel)
	}
	return dst, nil
}

// Contains reports (via a nil/err return) whether target — an ALREADY-joined
// path — stays under base, i.e. the base→target relative path is not a `..`
// escape. Unlike Join it does NOT reject target == base; callers that must also
// forbid the root use Join. Use this when the caller has already composed the
// absolute path and only needs the escape assertion.
func Contains(base, target string) error {
	r, err := filepath.Rel(base, target)
	if err != nil || r == ".." || strings.HasPrefix(r, dotDotPrefix) {
		return fmt.Errorf("path %q escapes %q", target, base)
	}
	return nil
}

// ValidateRel rejects a relative path that would not stay contained when joined
// under a root: an absolute path or a `..` traversal. Unlike Join it takes no
// base and PERMITS `.` (the root itself) — used to validate an output DIR that
// may legitimately be the project root.
func ValidateRel(rel string) error {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, dotDotPrefix) {
		return fmt.Errorf("path %q must be contained (no `..` / absolute escape)", rel)
	}
	if hasControlChar(clean) {
		return fmt.Errorf("path %q contains a control character", rel)
	}
	return nil
}
