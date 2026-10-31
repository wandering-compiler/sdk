package migratecli

import (
	"context"
	"strings"
	"testing"

	"path/filepath"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// --checks is an apply flag (refused elsewhere), and W17_MIGRATE_CHECKS turns
// it on for every apply of an environment.
func TestChecksFlagAndSetting(t *testing.T) {
	f, err := parseFlags("apply", []string{"--checks"}, &strings.Builder{})
	if err != nil || !f.checks {
		t.Fatalf("apply --checks: checks=%v, %v", f.checks, err)
	}
	if f, err = parseFlags("apply", nil, &strings.Builder{}); err != nil || f.checks {
		t.Fatalf("apply: checks on by default (%v, %v)", f.checks, err)
	}
	if _, err := parseFlags("status", []string{"--checks"}, &strings.Builder{}); err == nil {
		t.Fatal("status --checks accepted and ignored")
	}
	for v, want := range map[string]bool{"1": true, "true": true, " YES ": true, "": false, "0": false, "no": false} {
		if got := checksFromEnv(func(string) string { return v }); got != want {
			t.Errorf("W17_MIGRATE_CHECKS=%q → %v, want %v", v, got, want)
		}
	}
	if !strings.Contains(usage, "--checks") {
		t.Error("the apply usage does not mention --checks")
	}
}

// The real `migrate apply` entry, on a real SQLite store whose table holds a
// row the migration's check counts: --checks, or W17_MIGRATE_CHECKS=1,
// refuses the migration naming the check; without either it applies.
func TestApply_ChecksReachTheRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flag    bool
		env     string
		refused bool
	}{
		{"flag", true, "", true},
		{"setting", false, "1", true},
		{"neither", false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "app.db")
			seed := &applyfetchpb.Migration{Id: "ts-1", Connection: "app-sqlite",
				UpSql: `CREATE TABLE w17_migrations (timestamp TEXT PRIMARY KEY, content_sha256 TEXT NOT NULL); CREATE TABLE "c" (id INTEGER PRIMARY KEY, pid INTEGER); INSERT INTO "c" VALUES (1, 7); INSERT INTO w17_migrations VALUES ('ts-1', 'x');`}
			m := &applyfetchpb.Migration{Id: "ts-2", Connection: "app-sqlite",
				UpSql:  `ALTER TABLE "c" ADD COLUMN x INTEGER; INSERT INTO w17_migrations VALUES ('ts-2', 'y');`,
				Checks: []*applyfetchpb.Check{{Name: "fk_add_c_pid", Sql: `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`, Rationale: "every child row holding a value is an orphan"}}}
			seed.ContentSha256 = migrate.ContentHash(seed.GetUpSql(), "", "", "", "", nil, "", "")
			m.PrevContentSha256 = seed.GetContentSha256()
			m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), "", "", "", m.GetPrevContentSha256(), nil, "", "")
			dir := t.TempDir()
			for _, x := range []*applyfetchpb.Migration{seed, m} {
				if err := migrate.WriteMigration(dir, x); err != nil {
					t.Fatal(err)
				}
			}
			env := envMap{"W17_TARGET_APP_SQLITE": "sqlite://" + db, "W17_MIGRATE_CHECKS": tc.env}
			var out strings.Builder
			// the seed migration first, without checks, so the table has its row
			lock1 := writeLock(t, migrate.LockConnection{Name: "app-sqlite", TargetMigrationID: "ts-1", TargetContentSha256: seed.GetContentSha256()})
			if err := runApply(context.Background(), []string{"--lock", lock1, "--migrations", dir}, Options{Getenv: envMap{"W17_TARGET_APP_SQLITE": "sqlite://" + db}.get, Out: &out}, &out); err != nil {
				t.Fatalf("seed: %v\n%s", err, out.String())
			}
			lock := writeLock(t, migrate.LockConnection{Name: "app-sqlite", TargetMigrationID: "ts-2", TargetContentSha256: m.GetContentSha256()})
			args := []string{"--lock", lock, "--migrations", dir}
			if tc.flag {
				args = append(args, "--checks")
			}
			err := runApply(context.Background(), args, Options{Getenv: env.get, Out: &out}, &out)
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "fk_add_c_pid") || !strings.Contains(err.Error(), "1 row(s)") {
					t.Fatalf("want the check to refuse ts-2, got %v\n%s", err, out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("apply without checks: %v\n%s", err, out.String())
			}
		})
	}
}
