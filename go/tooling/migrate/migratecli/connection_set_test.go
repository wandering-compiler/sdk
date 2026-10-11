package migratecli

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Two connections whose names differ only in case or in `-` versus `_` read
// one DSN variable, so both would be opened against one database. Every verb
// that resolves DSNs refuses the set, naming both connections and the
// variable — before anything is opened.
func TestDSNVariable_TwoConnectionsCannotShareOne(t *testing.T) {
	lock := writeLock(t,
		migrate.LockConnection{Name: "app-db", TargetMigrationID: "ts-1"},
		migrate.LockConnection{Name: "app_db", TargetMigrationID: "ts-1"},
	)
	env := envMap{"W17_TARGET_APP_DB": "postgres://x/y"}.get
	for _, argv := range [][]string{
		{"migrate", "apply", "--lock", lock},
		{"migrate", "status", "--lock", lock},
		{"migrate", "rollback", "--to", "", "--lock", lock},
		{"fixtures", "apply", "--lock", lock, "--fixtures", t.TempDir()},
	} {
		t.Run(strings.Join(argv[:2], " "), func(t *testing.T) {
			var out bytes.Buffer
			_, err := Dispatch(context.Background(), argv, Options{Getenv: env, Out: &out})
			if err == nil {
				t.Fatal("two connections sharing W17_TARGET_APP_DB were accepted")
			}
			for _, want := range []string{"app-db", "app_db", "W17_TARGET_APP_DB"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// Distinct variables stay accepted, and so does one connection on its own.
func TestDSNVariable_DistinctNamesPass(t *testing.T) {
	if err := refuseSharedDSNVariable("migrate", []migrate.ConnTarget{
		{Connection: "app-db"}, {Connection: "app-db-replica"}, {Connection: "billing"},
	}); err != nil {
		t.Fatalf("distinct variables refused: %v", err)
	}
}

// An unknown `fixtures` subcommand answers with the FIXTURES usage — not the
// migrate one, whose verbs this command does not have — and fails.
func TestFixtures_UnknownSubcommandPrintsFixturesUsage(t *testing.T) {
	err := Fixtures(context.Background(), []string{"seed"}, Options{Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("an unknown fixtures subcommand succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, fixturesUsage) {
		t.Errorf("refusal does not carry the fixtures usage:\n%s", msg)
	}
	if strings.Contains(msg, usage) {
		t.Errorf("refusal carries the migrate usage:\n%s", msg)
	}
	if !strings.Contains(msg, `"seed"`) {
		t.Errorf("refusal does not name the word it did not recognise:\n%s", msg)
	}
}

// fetchConsole is a MigrationFetch server answering with a fixed set.
type fetchConsole struct {
	applyfetchpb.UnimplementedMigrationFetchServer
	reply []*applyfetchpb.Migration
}

func (f fetchConsole) FetchMigrations(context.Context, *applyfetchpb.FetchMigrationsRequest) (*applyfetchpb.FetchMigrationsResponse, error) {
	return &applyfetchpb.FetchMigrationsResponse{Migrations: f.reply}, nil
}

// serveConsole starts a plaintext console answering reply and returns an env
// pointing at it.
func serveConsole(t *testing.T, reply ...*applyfetchpb.Migration) func(string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	applyfetchpb.RegisterMigrationFetchServer(srv, fetchConsole{reply: reply})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return envMap{envConsoleAddr: "grpc://" + lis.Addr().String()}.get
}

// A connection the fetch did not ask for is not part of the result, so it is
// neither written beside this bundle's artefacts nor applied.
func TestFetch_KeepsOnlyRequestedConnections(t *testing.T) {
	env := serveConsole(t,
		&applyfetchpb.Migration{Connection: "app", Id: "20260101T000000Z"},
		&applyfetchpb.Migration{Connection: "other", Id: "20260101T000000Z"},
	)
	migs, err := fetchMigrations(context.Background(), "p-1",
		[]migrate.ConnTarget{{Connection: "app", TargetMigrationID: "20260101T000000Z"}}, env)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(migs) != 1 || migs[0].GetConnection() != "app" {
		t.Fatalf("fetched set = %v, want only connection app", migs)
	}
}

// A reply naming one (connection, id) twice is refused: both copies land on
// one path, and which body survived would depend on the reply's order.
func TestFetch_RefusesADuplicateMigration(t *testing.T) {
	env := serveConsole(t,
		&applyfetchpb.Migration{Connection: "app", Id: "20260101T000000Z", UpSql: "SELECT 1;"},
		&applyfetchpb.Migration{Connection: "app", Id: "20260101T000000Z", UpSql: "SELECT 2;"},
	)
	_, err := fetchMigrations(context.Background(), "p-1",
		[]migrate.ConnTarget{{Connection: "app", TargetMigrationID: "20260101T000000Z"}}, env)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate migration = %v, want a refusal", err)
	}
}

// The in-memory set refuses a duplicate on its own, so a caller that builds
// one from another source gets the same answer.
func TestMigrationSetFS_RefusesADuplicate(t *testing.T) {
	_, err := migrate.MigrationSetFS([]*applyfetchpb.Migration{
		{Connection: "app", Id: "20260101T000000Z"},
		{Connection: "app", Id: "20260101T000000Z"},
	})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("MigrationSetFS with a duplicate = %v, want a refusal", err)
	}
}
