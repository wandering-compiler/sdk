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
// already ran (`w17ctl migrate replace`). A database that cannot be read, a
// fresh one, and an unpinned connection report nothing — and never fail the
// fetch.
func TestAppliedHeads_ReportsWhatEachDatabaseApplied(t *testing.T) {
	appliers := map[string]*stub.Applier{
		"main":   {Head: "0002"},
		"fresh":  {},
		"broken": {HeadErr: errors.New("connection refused")},
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
	})
	if len(heads) != 1 || heads[0].GetConnection() != "main" || heads[0].GetMigrationId() != "0002" {
		t.Fatalf("heads = %v, want only main@0002", heads)
	}
}

// The heads travel on the fetch request itself.
func TestFetch_SendsTheAppliedHeads(t *testing.T) {
	f := &fakeFetch{}
	addr := serveFetch(t, f, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sent := []*applyfetchpb.AppliedHead{{Connection: "main", MigrationId: "0001"}}
	if _, err := fetchMigrationsReporting(ctx, "p-1", pinned, sent, fetchEnv(map[string]string{envConsoleAddr: "grpc://" + addr})); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.heads) != 1 || len(f.heads[0]) != 1 || f.heads[0][0].GetMigrationId() != "0001" {
		t.Fatalf("the console received heads %v, want main@0001", f.heads)
	}
}
