package postgres

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Snapshotter is the PG dump/restore driver for the dev DB
// lifecycle. It shells out to the stock `pg_dump` / `psql` client
// binaries rather than reimplementing the dump format over pgx: a
// snapshot must round-trip the *whole* database (tables, data,
// sequences, indexes, the wc_migrations ledger), which pg_dump
// already does correctly and is the precedent the spec names
// (`docs/specs/storage/dev-db-lifecycle.md` S1). The binaries are a
// reasonable dependency for a developer-machine tool (w17ctl); the
// factory only constructs this on the dev snapshot path, never on the
// production apply path.
//
// Dump uses `--clean --if-exists`, so the emitted script first DROPs
// each object (guarded by IF EXISTS) and then recreates it. That makes
// Restore idempotent against both a freshly-wiped DB and one that
// still holds a previous snapshot's objects — the branch-switch
// reconcile needs both.
type Snapshotter struct {
	dsn string

	// dumpBin / restoreBin name the client binaries. Defaulted by
	// NewSnapshotter; overridable in tests so the round-trip can point
	// at a versioned client on PATH.
	dumpBin    string
	restoreBin string
}

// Compile-time check the impl satisfies the snapshot contract.
var _ migrate.Snapshotter = (*Snapshotter)(nil)

// NewSnapshotter builds a PG Snapshotter for a connection DSN.
// Unlike the Applier it opens no connection — pg_dump / psql dial the
// DSN themselves per invocation — so construction only validates the
// DSN is non-empty (typos surface when the client process runs, with
// the client's own connection diagnostics).
func NewSnapshotter(dsn string) (*Snapshotter, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres.NewSnapshotter: dsn is empty")
	}
	return &Snapshotter{dsn: dsn, dumpBin: "pg_dump", restoreBin: "psql"}, nil
}

// conn returns the `--dbname` value to pass the client binary plus
// the child environment. When the DSN is a URL carrying a password,
// the password is routed via the PGPASSWORD environment variable and
// stripped from the returned URL so it never appears on the process'
// argv (visible to anyone running `ps`) — mirroring the MySQL
// snapshotter's MYSQL_PWD handling. Any other DSN form (libpq
// keyword/value, or a URL with no password) is passed through
// unchanged with the inherited environment (env == nil → exec
// inherits os.Environ()).
func (s *Snapshotter) conn() (dbname string, env []string) {
	u, err := url.Parse(s.dsn)
	if err != nil || u.User == nil {
		return s.dsn, nil
	}
	pw, ok := u.User.Password()
	if !ok {
		return s.dsn, nil
	}
	u.User = url.User(u.User.Username())
	return u.String(), append(os.Environ(), "PGPASSWORD="+pw)
}

// Dump streams a plain-SQL `pg_dump --clean --if-exists` of the whole
// database to w. `--no-owner` / `--no-privileges` keep the script
// portable across the throwaway dev roles a snapshot is restored under
// (the dev DB owner on the incoming branch need not match the one the
// dump was taken under). pg_dump's own stderr is captured and folded
// into the error so a failure (unreachable DB, missing binary) surfaces
// the why.
func (s *Snapshotter) Dump(ctx context.Context, w io.Writer) error {
	dbname, env := s.conn()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.dumpBin,
		"--clean", "--if-exists",
		"--no-owner", "--no-privileges",
		"--format=plain",
		"--dbname="+dbname,
	)
	cmd.Env = env
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("postgres Dump (%s): %w: %s", s.dumpBin, err, stderr.String())
	}
	return nil
}

// Restore replays a Dump stream into the target DB via psql.
// `ON_ERROR_STOP=1` makes psql exit non-zero on the first failing
// statement (default psql plows on, masking a botched restore);
// `--no-psqlrc` keeps a developer's ~/.psqlrc from perturbing the
// session. The `--clean --if-exists` header the dump carries means the
// replay overwrites whatever the target currently holds.
func (s *Snapshotter) Restore(ctx context.Context, r io.Reader) error {
	dbname, env := s.conn()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.restoreBin,
		"--quiet", "--no-psqlrc",
		"-v", "ON_ERROR_STOP=1",
		"--dbname="+dbname,
	)
	cmd.Env = env
	cmd.Stdin = r
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("postgres Restore (%s): %w: %s", s.restoreBin, err, stderr.String())
	}
	return nil
}
