package sqlitecollate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// StampTable holds the text-ordering provenance of whoever built this
// database's indexes.
//
// It sits next to `wc_migrations` because that is what it describes: the
// stamp belongs where the index it characterises lives, so it survives an
// independent redeploy of either the applier or the runtime. PostgreSQL
// solves the same problem the same way, with `pg_collation.collversion`
// alongside the catalog.
const StampTable = "wc_collation"

// EnsureStamp records this binary's collator on a database that has none,
// and reports a mismatch on one that was built with a different collator.
// For the WRITE side — the migration applier — because applying DDL under
// a foreign collator is the act that builds a mismatched index.
//
// T1-4 pass #12, B-F5. [Fingerprint] explains why a mismatch is possible
// at all; the short version is that the guarantee this package advertises
// holds within one BUILD, and the applier, the dev-DB snapshotter and each
// generated runtime are separate binaries.
func EnsureStamp(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS `+StampTable+` (
			id          INTEGER PRIMARY KEY CHECK (id = 1),
			fingerprint TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("collation stamp: ensure %s: %w", StampTable, err)
	}
	stored, ok, err := readStamp(ctx, db)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO `+StampTable+` (id, fingerprint) VALUES (1, ?)`, Fingerprint()); err != nil {
			return fmt.Errorf("collation stamp: write: %w", err)
		}
		return nil
	}
	return compareStamp(stored)
}

// VerifyStamp reports a mismatch between this binary's collator and the one
// that built the database's indexes. For the READ side — a generated
// service binary at connect — which is where a mismatch turns into wrong
// answers: an index sorted by one collator and searched by another silently
// misses rows.
//
// An ABSENT stamp is not an error. A database that predates stamping, or
// one no migration has touched, has nothing to disagree with, and refusing
// to serve it would turn a detector into an outage. The check exists to
// catch a KNOWN disagreement, not to require provenance.
func VerifyStamp(ctx context.Context, db *sql.DB) error {
	stored, ok, err := readStamp(ctx, db)
	if err != nil || !ok {
		// A missing table reads as "no stamp" via readStamp; a genuine
		// read failure is reported by it.
		return err
	}
	return compareStamp(stored)
}

func readStamp(ctx context.Context, db *sql.DB) (string, bool, error) {
	var stored string
	err := db.QueryRowContext(ctx,
		`SELECT fingerprint FROM `+StampTable+` WHERE id = 1`).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		// SQLite reports an absent table as an error string rather than a
		// typed code (modernc exposes none), and an unstamped database is
		// the ordinary case for anything that predates this check — so a
		// missing table is "no stamp", not a failure.
		if isMissingTable(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("collation stamp: read: %w", err)
	}
	return stored, true, nil
}

func compareStamp(stored string) error {
	mine := Fingerprint()
	if stored == mine {
		return nil
	}
	return fmt.Errorf(
		"this database's indexes were built with a different text collator\n"+
			"  stored:      %s\n"+
			"  this binary: %s\n"+
			"  why: the ordering an index was built with must be the ordering queries compare with, or index-backed lookups silently miss rows; the two Unicode table versions in the stamp (scheme:go-unicode:x-text:digest) say which side moved\n"+
			"  fix: rebuild the applier and the service binaries from one dependency set, or rebuild the affected indexes under the current collator",
		stored, mine)
}

func isMissingTable(err error) bool {
	return err != nil && containsFold(err.Error(), "no such table")
}

func containsFold(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
