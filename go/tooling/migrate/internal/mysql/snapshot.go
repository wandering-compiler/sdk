package mysql

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Snapshotter is the MySQL dump/restore driver for the dev DB
// lifecycle. Like the PG snapshotter it shells out to the stock
// client binaries (`mysqldump` / `mysql`) rather than reimplementing
// the dump format over database/sql — mysqldump round-trips the whole
// schema database (tables, data, the wc_migrations ledger) correctly.
//
// mysqldump defaults to `--add-drop-table`, so the emitted script
// DROPs each table before recreating it, making Restore idempotent
// against a populated DB. `--single-transaction` takes a consistent
// snapshot of InnoDB tables without locking (the reconcile also
// quiesces writers, so this is belt-and-braces). `--no-tablespaces`
// avoids the PROCESS privilege a default dump would demand.
type Snapshotter struct {
	host, port, user, pass, db string

	dumpBin    string
	restoreBin string
}

var _ migrate.Snapshotter = (*Snapshotter)(nil)

// NewSnapshotter parses the URL-shaped DSN into the connection
// components mysqldump / mysql consume as flags. The password is
// passed to the child via the MYSQL_PWD environment variable rather
// than `--password=` so it does not appear in the process table.
func NewSnapshotter(dsn string) (*Snapshotter, error) {
	if dsn == "" {
		return nil, fmt.Errorf("mysql.NewSnapshotter: dsn is empty")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql.NewSnapshotter: parse url: %w", err)
	}
	if u.Scheme != "mysql" {
		return nil, fmt.Errorf("mysql.NewSnapshotter: expected mysql:// scheme, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("mysql.NewSnapshotter: missing host")
	}
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		return nil, fmt.Errorf("mysql.NewSnapshotter: missing database name in DSN path")
	}
	s := &Snapshotter{
		host: host, port: port, db: db,
		dumpBin: "mysqldump", restoreBin: "mysql",
	}
	if u.User != nil {
		s.user = u.User.Username()
		s.pass, _ = u.User.Password()
	}
	return s, nil
}

// connFlags are the shared `--host/--port/--user` flags both client
// binaries accept.
func (s *Snapshotter) connFlags() []string {
	args := []string{"--host=" + s.host, "--port=" + s.port}
	if s.user != "" {
		args = append(args, "--user="+s.user)
	}
	return args
}

// childEnv carries MYSQL_PWD so the password never hits the argv.
func (s *Snapshotter) childEnv() []string {
	env := os.Environ()
	if s.pass != "" {
		env = append(env, "MYSQL_PWD="+s.pass)
	}
	return env
}

// Dump streams a `mysqldump` of the schema database to w.
func (s *Snapshotter) Dump(ctx context.Context, w io.Writer) error {
	args := append(s.connFlags(),
		"--single-transaction",
		"--no-tablespaces",
		"--add-drop-table",
		s.db,
	)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.dumpBin, args...)
	cmd.Env = s.childEnv()
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mysql Dump (%s): %w: %s", s.dumpBin, err, stderr.String())
	}
	return nil
}

// Restore replays a Dump stream into the schema database via the
// `mysql` client. The DROP/CREATE header in the dump overwrites
// whatever the target currently holds.
func (s *Snapshotter) Restore(ctx context.Context, r io.Reader) error {
	args := append(s.connFlags(), s.db)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.restoreBin, args...)
	cmd.Env = s.childEnv()
	cmd.Stdin = r
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mysql Restore (%s): %w: %s", s.restoreBin, err, stderr.String())
	}
	return nil
}
