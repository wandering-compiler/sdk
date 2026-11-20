package migrate

import (
	"context"
	"fmt"
)

// SeedTx is the transaction ExecSeedFiles runs a seed in.
type SeedTx interface {
	Exec(ctx context.Context, sql string, args ...any) error
}

// ExecSeedFiles runs a seed's statements inside tx, a fixture at a time, in
// an order the foreign keys accept.
//
// The renderer orders rows by foreign key WITHIN a fixture, never between
// fixtures, and a seed is the fixtures of a domain in name order — so a row
// referencing another fixture's row applied or failed by file NAME
// (`org-hosts` before `acme-org`, `org-invites` before `org`):
// on a fresh database, a restore, or a deploy whose new organization and the
// rows that need it arrive in two files.
//
// Each fixture (a run of statements with one SeedStmt.File) runs under a
// savepoint. One that fails on an error deferrable reports — a foreign key
// pointing at a row not there yet — is rolled back to its savepoint and run
// again after the others; the passes repeat while any fixture lands. When a
// pass lands none, the first deferred fixture's error is returned, naming it:
// a parent that no fixture creates is still a failure, as before. Any other
// error fails at once. The caller owns the transaction, so the seed is still
// all or nothing.
func ExecSeedFiles(ctx context.Context, tx SeedTx, stmts []SeedStmt, deferrable func(error) bool) error {
	type fixture struct {
		file  string
		first int // index of its first statement in stmts
		stmts []SeedStmt
	}
	var files []fixture
	for i, st := range stmts {
		if n := len(files); n > 0 && files[n-1].file == st.File {
			files[n-1].stmts = append(files[n-1].stmts, st)
			continue
		}
		files = append(files, fixture{file: st.File, first: i, stmts: []SeedStmt{st}})
	}
	run := func(f fixture) error {
		for j, st := range f.stmts {
			if err := tx.Exec(ctx, st.SQL, st.Args...); err != nil {
				where := fmt.Sprintf("statement %d of %d", f.first+j+1, len(stmts))
				if f.file != "" {
					where = "fixture " + f.file + ", " + where
				}
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		return nil
	}
	if len(files) < 2 {
		for _, f := range files {
			if err := run(f); err != nil {
				return err
			}
		}
		return nil
	}
	pending := files
	for len(pending) > 0 {
		var deferred []fixture
		var firstErr error
		for _, f := range pending {
			if err := tx.Exec(ctx, "SAVEPOINT w17_seed_fixture"); err != nil {
				return fmt.Errorf("savepoint: %w", err)
			}
			if err := run(f); err != nil {
				if deferrable == nil || !deferrable(err) {
					return err
				}
				if rerr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT w17_seed_fixture"); rerr != nil {
					return fmt.Errorf("%w (and rolling it back: %w)", err, rerr)
				}
				deferred = append(deferred, f)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := tx.Exec(ctx, "RELEASE SAVEPOINT w17_seed_fixture"); err != nil {
				return fmt.Errorf("release savepoint: %w", err)
			}
		}
		if len(deferred) == len(pending) {
			return firstErr
		}
		pending = deferred
	}
	return nil
}
