package migratecli

import (
	"context"
	"errors"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// A fetch made from where the database is reports what that database has
// applied, so the console can refuse to replace a migration some environment
// already ran (`w17ctl migrate replace`). A database that cannot be read and
// an unpinned connection report nothing — and never fail the fetch. Every
// report names its database, and a database with nothing applied reports an
// empty head: the console judges each database by its LATEST report, so one
// rolled all the way back must say so.
func TestAppliedHeads_ReportsWhatEachDatabaseApplied(t *testing.T) {
	appliers := map[string]*stub.Applier{
		"main":   {Head: "0002"},
		"fresh":  {},
		"broken": {HeadErr: errors.New("connection refused")},
	}
	dsns := map[string]string{
		"main":  "postgres://deploy:secret@db.prod:5432/app?sslmode=require",
		"fresh": "postgres://db.prod:5432/other",
	}
	targets := []migrate.ConnTarget{
		{Connection: "broken", TargetMigrationID: "0001"},
		{Connection: "fresh", TargetMigrationID: "0001"},
		{Connection: "main", TargetMigrationID: "0002"},
		{Connection: "unpinned"},
	}
	heads := appliedHeads(context.Background(), targets, func(conn string) (migrate.Applier, error) {
		a, ok := appliers[conn]
		if !ok {
			return nil, errors.New("no DSN for " + conn)
		}
		return a, nil
	}, func(conn string) string { return dsns[conn] })
	if len(heads) != 2 {
		t.Fatalf("heads = %v, want fresh (empty head) and main@0002", heads)
	}
	if heads[0].GetConnection() != "fresh" || heads[0].GetMigrationId() != "" || heads[0].GetDatabaseId() == "" {
		t.Errorf("fresh = %v, want an empty head WITH its database id", heads[0])
	}
	if heads[1].GetConnection() != "main" || heads[1].GetMigrationId() != "0002" || heads[1].GetDatabaseId() != databaseID(dsns["main"]) {
		t.Errorf("main = %v, want 0002 with its database id", heads[1])
	}
	if heads[0].GetDatabaseId() == heads[1].GetDatabaseId() {
		t.Error("two databases on one server share an id")
	}
}

// Without an id an empty head says nothing, so it is not sent.
func TestAppliedHeads_AnEmptyHeadWithoutAnIdIsNotSent(t *testing.T) {
	heads := appliedHeads(context.Background(),
		[]migrate.ConnTarget{{Connection: "main", TargetMigrationID: "0001"}},
		func(string) (migrate.Applier, error) { return &stub.Applier{}, nil }, nil)
	if len(heads) != 0 {
		t.Fatalf("heads = %v, want none", heads)
	}
}

// The id is where the database is: the same for every credential and query
// string, different for another host or database.
func TestDatabaseID_IsTheLocationWithoutCredentials(t *testing.T) {
	a := databaseID("postgres://deploy:one@DB.prod:5432/app?sslmode=require")
	for _, same := range []string{
		"postgresql://other:two@db.prod:5432/app",
		"postgres://db.prod:5432/app?application_name=x",
	} {
		if databaseID(same) != a {
			t.Errorf("%s: id differs from the same database reached with other credentials", same)
		}
	}
	for _, other := range []string{
		"postgres://db.staging:5432/app",
		"postgres://db.prod:5432/app2",
		"mysql://db.prod:5432/app",
	} {
		if databaseID(other) == a {
			t.Errorf("%s: shares an id with another database", other)
		}
	}
	if databaseID("") != "" || databaseID("::not a url") != "" {
		t.Error("an unusable URL must give no id")
	}
	if databaseID("file:app.db") == "" || databaseID("sqlite:///var/lib/app.db") == "" {
		t.Error("a SQLite file is a location too")
	}
}

// The heads travel on the fetch request itself, and what the console says
// was replaced comes back for the plan.
func TestFetch_SendsTheAppliedHeadsAndReturnsWhatWasReplaced(t *testing.T) {
	f := &fakeFetch{resp: &applyfetchpb.FetchMigrationsResponse{
		Replaced: []*applyfetchpb.ReplacedMigration{{Connection: "main", MigrationId: "0001", ReplacedBy: "0003"}},
	}}
	addr := serveFetch(t, f, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sent := []*applyfetchpb.AppliedHead{{Connection: "main", MigrationId: "0001", DatabaseId: "db-1"}}
	_, replaced, err := fetchMigrationsReporting(ctx, "p-1", pinned, sent, fetchEnv(map[string]string{envConsoleAddr: "grpc://" + addr}))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.heads) != 1 || len(f.heads[0]) != 1 || f.heads[0][0].GetMigrationId() != "0001" || f.heads[0][0].GetDatabaseId() != "db-1" {
		t.Fatalf("the console received heads %v, want main@0001 from db-1", f.heads)
	}
	if replaced["main"]["0001"] != "0003" {
		t.Errorf("replaced = %v, want main: 0001 → 0003", replaced)
	}
}
