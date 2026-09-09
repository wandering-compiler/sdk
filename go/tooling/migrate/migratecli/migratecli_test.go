package migratecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// writeLock plants a lock with the given connections + pins.
func writeLock(t *testing.T, conns ...migrate.LockConnection) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("project: demo\nproject_id: p-1\nconnections:\n")
	for _, c := range conns {
		b.WriteString("  - name: " + c.Name + "\n")
		if c.TargetMigrationID != "" {
			b.WriteString("    target_migration_id: " + c.TargetMigrationID + "\n")
			b.WriteString("    target_content_sha256: " + c.TargetContentSha256 + "\n")
		}
	}
	path := filepath.Join(t.TempDir(), "lock.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A bundle migrates ITS connections and no others. Two storage bundles in one
// project each hold a slice of the lock, and one reaching past its own would
// run DDL against a database it does not serve.
func TestOwnedTargets_NarrowsToTheBundlesOwnConnections(t *testing.T) {
	all := []migrate.ConnTarget{
		{Connection: "app-postgres", TargetMigrationID: "ts-1"},
		{Connection: "billing-postgres", TargetMigrationID: "ts-2"},
	}
	got := ownedTargets(all, []string{"app-postgres"})
	if len(got) != 1 || got[0].Connection != "app-postgres" {
		t.Fatalf("got %+v, want only app-postgres", got)
	}
}

// An empty owned set means "everything" — a single-bundle project, or a test
// harness that has no reason to enumerate.
func TestOwnedTargets_EmptyMeansAll(t *testing.T) {
	all := []migrate.ConnTarget{{Connection: "a"}, {Connection: "b"}}
	if got := ownedTargets(all, nil); len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

// A bundle whose connections match NOTHING in the lock must fail loudly. This
// is the arm that would otherwise print "nothing pending" and exit 0 — a
// wiring mistake wearing the costume of a clean deploy, which is precisely
// the failure shape this whole change exists to end.
func TestBuildConfig_NoMatchingConnectionIsAnError(t *testing.T) {
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres", TargetMigrationID: "ts-1"})
	_, err := buildConfig(
		context.Background(),
		applyFlags{lock: lock, migrations: "w17/migrations", logFormat: "text"},
		Options{Connections: []string{"typo-postgres"}},
		os.Stdout,
	)
	if err == nil {
		t.Fatal("a bundle matching no connection in the lock must be an error, not an empty run")
	}
	for _, want := range []string{"typo-postgres", "none of this bundle's connections"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name what did not match (%q): %v", want, err)
		}
	}
}

// A missing DSN names the connection AND the variable to set. "no DSN for
// connection app-postgres" alone sends the reader looking for a config file
// that does not exist.
func TestDsnSpecs_MissingDSNNamesTheVariable(t *testing.T) {
	_, err := dsnSpecs(
		[]migrate.ConnTarget{{Connection: "read-replica", TargetMigrationID: "ts-1"}},
		func(string) string { return "" },
	)
	if err == nil {
		t.Fatal("want a missing-DSN error")
	}
	if !strings.Contains(err.Error(), "W17_TARGET_READ_REPLICA") {
		t.Errorf("error should name the env var to set: %v", err)
	}
}

// An unpinned connection is skipped rather than demanded. It has never been
// schema-pushed, so there is nothing to apply — insisting on its DSN would
// block a deploy on a database nobody is migrating.
func TestDsnSpecs_UnpinnedConnectionNeedsNoDSN(t *testing.T) {
	specs, err := dsnSpecs(
		[]migrate.ConnTarget{
			{Connection: "pinned", TargetMigrationID: "ts-1"},
			{Connection: "never-pushed"},
		},
		func(k string) string {
			if k == "W17_TARGET_PINNED" {
				return "postgres://x"
			}
			return ""
		},
	)
	if err != nil {
		t.Fatalf("an unpinned connection must not demand a DSN: %v", err)
	}
	if len(specs) != 1 || specs[0].Connection != "pinned" {
		t.Fatalf("got %+v, want only the pinned connection", specs)
	}
}

// DSNs come from the environment, never a flag. A credential in a flag lands
// in shell history and in every process listing on the host, and the place
// that guarantee can quietly disappear is a well-meaning `--dsn` convenience.
func TestFlags_OfferNoWayToPassADSN(t *testing.T) {
	var sb strings.Builder
	_, err := parseFlags("apply", []string{"--dsn", "postgres://u:p@h/db"}, &sb)
	if err == nil {
		t.Fatal("a --dsn flag must not exist")
	}
	if strings.Contains(usage, "--dsn") {
		t.Error("usage advertises a --dsn flag")
	}
}

func TestMain_UnknownSubcommandIsSteered(t *testing.T) {
	err := Main(context.Background(), []string{"aply"}, Options{Out: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("want an unknown-subcommand refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "apply") {
		t.Errorf("refusal should name the real subcommands: %v", err)
	}
}

func TestMain_NoSubcommandIsSteered(t *testing.T) {
	err := Main(context.Background(), nil, Options{Out: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "subcommand is required") {
		t.Fatalf("want a missing-subcommand refusal, got %v", err)
	}
}

func TestParseFlags_RejectsBadLogFormat(t *testing.T) {
	var sb strings.Builder
	if _, err := parseFlags("apply", []string{"--log-format", "yaml"}, &sb); err == nil {
		t.Fatal("want a refusal for an unsupported log format")
	}
}

// --- rollback -------------------------------------------------------

// `--to` is REQUIRED, and forgetting it must not mean "roll back everything".
// The empty string is a legal VALUE that says exactly that, and the difference
// between typing it and omitting the flag is the difference between a decision
// and an accident on a destructive command.
func TestRollback_RefusesWithoutTo(t *testing.T) {
	var out strings.Builder
	err := Main(context.Background(), []string{"rollback"}, Options{Out: &out})
	if err == nil {
		t.Fatal("rollback without --to must be refused")
	}
	if !strings.Contains(err.Error(), "--to is required") {
		t.Errorf("refusal should name the missing flag: %v", err)
	}
	// And it must explain the empty-string form, or the operator who genuinely
	// wants to undo everything has no way to say so.
	if !strings.Contains(err.Error(), `--to ""`) {
		t.Errorf("refusal should show how to mean 'everything': %v", err)
	}
}

// An EXPLICIT --to="" is accepted — it reaches the orchestrator rather than
// being rejected as empty. Without this the refusal above would have made
// "roll back everything" unreachable, which is a worse bug than the one it
// prevents.
func TestRollback_ExplicitEmptyToIsAccepted(t *testing.T) {
	f, err := parseFlags("rollback", []string{"--to", ""}, &strings.Builder{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.toSet {
		t.Error("an explicitly passed --to=\"\" must register as SET, not as absent")
	}
	if f.to != "" {
		t.Errorf("to = %q, want empty", f.to)
	}
}

// rollback is a sibling of apply, not a mode of it: same level, other
// direction. A flag on apply would put "undo" one typo away from "do".
func TestRollback_IsItsOwnSubcommand(t *testing.T) {
	if !strings.Contains(usage, "rollback ") {
		t.Error("usage does not list rollback as a command")
	}
	var out strings.Builder
	err := Main(context.Background(), []string{"rollback"}, Options{Out: &out})
	if err == nil || strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("rollback must be a recognised subcommand, got %v", err)
	}
}
