package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// The console's REPLACED list on disk.
//
// `apply --fetch` receives FetchMigrationsResponse.replaced with the
// migrations and hands it to Plan as Config.Replaced. The two-step form —
// `migrate fetch` to a directory, then `migrate apply --migrations` — used to
// drop it: only the artefacts were written, so the apply had no way to tell a
// head the console replaced from an unknown past, and a replaced FIRST
// migration (whose replacement roots the chain, below which an unknown past
// gets the work for real) had the replacement run on top of it.
//
// The list is written beside the connection directories, never inside one:
// the loader parses every `*.json` in `<root>/<connection>/` as a Migration,
// so a file there would be refused as a malformed artefact.
//
// It is not bound by the lock or by any content hash, and it does not need to
// be: it can only ADD refusals. Deleting or editing it removes a guard, which
// whoever can write the directory can do to the artefacts as well.

// ReplacedFileName is the file beside the connection directories of a
// fetched migration set that carries the console's replaced list.
const ReplacedFileName = "w17-replaced.json"

const replacedFileVersion = 1

type replacedFile struct {
	Version int `json:"version"`
	// Connections maps connection → replaced migration id → the id that
	// replaced it. A connection present with no entries was fetched from a
	// console that replaced nothing on it; a connection absent was never
	// fetched into this directory by a binary that writes the list.
	Connections map[string]map[string]string `json:"connections"`
}

// ReadReplaced reads the replaced list from the root of a migration set.
// present is false when the set carries none (fetched by an older binary,
// assembled by hand, or held in memory); the map then is nil. A file that is
// there but cannot be read or parsed is an error — a guard that silently
// reads as "nothing replaced" is the failure this file exists to close.
func ReadReplaced(fsys fs.FS) (replaced map[string]map[string]string, present bool, err error) {
	buf, err := fs.ReadFile(fsys, ReplacedFileName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", ReplacedFileName, err)
	}
	var f replacedFile
	if err := json.Unmarshal(buf, &f); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w — re-run `migrate fetch` into this directory to rewrite it", ReplacedFileName, err)
	}
	if f.Version != replacedFileVersion {
		return nil, false, fmt.Errorf("%s has version %d, this binary reads version %d — re-run `migrate fetch` with this binary", ReplacedFileName, f.Version, replacedFileVersion)
	}
	if f.Connections == nil {
		f.Connections = map[string]map[string]string{}
	}
	return f.Connections, true, nil
}

// WriteReplaced records, under root, the replaced list a fetch received for
// the connections it fetched. Entries of OTHER connections already in the
// file are kept: two bundles of one project may fetch their own connections
// into one directory, and the second must not erase the first one's guard.
// Every fetched connection is recorded, with no entries when nothing on it was
// replaced, so a reader can tell "asked, nothing replaced" from "never asked".
func WriteReplaced(root string, fetched []string, replaced map[string]map[string]string) error {
	path := filepath.Join(root, ReplacedFileName)
	cur, _, err := ReadReplaced(os.DirFS(root))
	if err != nil {
		// An unreadable list is rewritten from what this fetch knows; the
		// other connections' entries in it are lost, which the next fetch of
		// those connections restores.
		cur = nil
	}
	if cur == nil {
		cur = map[string]map[string]string{}
	}
	for _, c := range fetched {
		if err := checkArtifactSegment("connection", c); err != nil {
			return fmt.Errorf("WriteReplaced: %w", err)
		}
		entries := map[string]string{}
		for id, by := range replaced[c] {
			entries[id] = by
		}
		cur[c] = entries
	}
	buf, err := json.MarshalIndent(replacedFile{Version: replacedFileVersion, Connections: cur}, "", "  ")
	if err != nil {
		return fmt.Errorf("WriteReplaced: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", root, err)
	}
	tmp, err := os.CreateTemp(root, "."+ReplacedFileName+".*")
	if err != nil {
		return fmt.Errorf("WriteReplaced: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(buf, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("WriteReplaced: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("WriteReplaced: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("WriteReplaced: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("WriteReplaced: %w", err)
	}
	return nil
}

// UnrecordedConnections names the connections among conns that the replaced
// list of a migration set does not cover — all of them when the set carries
// no list. Sorted, for a stable message.
func UnrecordedConnections(fsys fs.FS, conns []string) ([]string, error) {
	rep, _, err := ReadReplaced(fsys)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range conns {
		if _, ok := rep[c]; !ok {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

// replacedOf is the replaced list Plan and Run judge a set by: the one the
// caller passed (Config.Replaced, from a fetch held in memory), else the one
// the set carries on disk.
func (c Config) replacedOf(fsys fs.FS) (map[string]map[string]string, error) {
	if c.Replaced != nil {
		return c.Replaced, nil
	}
	rep, _, err := ReadReplaced(fsys)
	if err != nil {
		return nil, fmt.Errorf("the set's replaced list: %w", err)
	}
	return rep, nil
}
