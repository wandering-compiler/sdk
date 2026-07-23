package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/sqlitecollate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Snapshotter is the SQLite dump/restore driver for the dev DB
// lifecycle. SQLite is a single file, so the spec calls for a "file
// copy" (`docs/specs/storage/dev-db-lifecycle.md` S2). We take that a
// step further than a raw byte copy: Dump runs `VACUUM INTO` to
// materialise a clean, single-file image of the database, so the
// snapshot is consistent regardless of the live DB's journal mode
// (WAL leaves a `-wal`/`-shm` sidecar that a naive copy would miss).
// Restore writes that image back over the DB path and clears any stale
// WAL/SHM sidecars so the restored file is authoritative.
type Snapshotter struct {
	path string
}

var _ migrate.Snapshotter = (*Snapshotter)(nil)

// NewSnapshotter resolves the DB file path from the DSN. An in-memory
// DSN has no file to snapshot and is refused (dev snapshots are about
// on-disk branch state; `:memory:` is process-scoped and disappears
// anyway).
func NewSnapshotter(dsn string) (*Snapshotter, error) {
	if dsn == "" {
		return nil, fmt.Errorf("sqlite.NewSnapshotter: dsn is empty")
	}
	// Reject in-memory DSNs in any of their forms — `:memory:`,
	// `file::memory:`, or a `mode=memory` query param. They have no
	// on-disk file to snapshot and vanish with the process anyway.
	if strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory") {
		return nil, fmt.Errorf("sqlite.NewSnapshotter: in-memory DSN %q has no file to snapshot", dsn)
	}
	path := dsnToFilePath(dsn)
	if path == "" {
		return nil, fmt.Errorf("sqlite.NewSnapshotter: pathless DSN %q has no file to snapshot", dsn)
	}
	return &Snapshotter{path: path}, nil
}

// Dump materialises a clean single-file image via `VACUUM INTO` and
// streams it to w. The temp image lives beside the source DB (same
// directory → same filesystem, so VACUUM INTO never crosses a mount)
// and is removed afterwards.
func (s *Snapshotter) Dump(ctx context.Context, w io.Writer) error {
	// F8-D-3: register the W17_UNICODE collation before opening — `VACUUM INTO`
	// replays the source schema DDL, so a DB whose string columns carry
	// `COLLATE W17_UNICODE` (every generated SQLite project since F7-A-5) fails
	// with "no such collation sequence" unless this connection knows it. The
	// Snapshotter opens its OWN connection, distinct from the Applier's, so it
	// must register too (global + idempotent).
	sqlitecollate.Register()
	db, err := sql.Open("sqlite", "file:"+s.path)
	if err != nil {
		return fmt.Errorf("sqlite Dump: open: %w", err)
	}
	defer func() { _ = db.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".w17snap-*.db")
	if err != nil {
		return fmt.Errorf("sqlite Dump: temp image: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	// VACUUM INTO requires the target file NOT exist; CreateTemp made
	// an empty one, so drop it first. Always clean the image up.
	_ = os.Remove(tmpPath)
	defer func() { _ = os.Remove(tmpPath) }()

	// #nosec G202 -- tmpPath is a server-side os.CreateTemp name escaped via
	// sqliteQuote (single-quote doubling); it is never user-supplied SQL.
	// SQLite's VACUUM INTO takes a string literal, not a bind parameter.
	if _, err := db.ExecContext(ctx, `VACUUM INTO `+sqliteQuote(tmpPath)); err != nil {
		return fmt.Errorf("sqlite Dump: VACUUM INTO: %w", err)
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("sqlite Dump: read image: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("sqlite Dump: stream image: %w", err)
	}
	return nil
}

// Restore overwrites the DB file with the snapshot image and removes
// any stale `-wal`/`-shm` sidecars so the written file is the single
// source of truth (a leftover WAL would otherwise replay over it).
func (s *Snapshotter) Restore(_ context.Context, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("sqlite Restore: mkdir: %w", err)
	}
	// Write to a sibling temp file then rename, so a failed/partial
	// read never leaves a half-written DB at the live path.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".w17restore-*.db")
	if err != nil {
		return fmt.Errorf("sqlite Restore: temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sqlite Restore: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sqlite Restore: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sqlite Restore: rename: %w", err)
	}
	for _, sidecar := range []string{s.path + "-wal", s.path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("sqlite Restore: clear %s: %w", sidecar, err)
		}
	}
	return nil
}

// dsnToFilePath reduces a w17 SQLite DSN to the bare file path:
// strips the URL scheme via the existing URLToDriverDSN, drops a
// driver-native `file:` prefix, and cuts any `?query` suffix.
func dsnToFilePath(dsn string) string {
	p := URLToDriverDSN(dsn)
	p = strings.TrimPrefix(p, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

// sqliteQuote wraps a path in single quotes for the `VACUUM INTO`
// statement, doubling any embedded single quote (SQL string-literal
// escaping). Paths with quotes are pathological but cheap to handle.
func sqliteQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "''") + "'"
}
