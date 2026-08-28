package migrate_test

import (
	"context"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// The apply half of squash: convincing a LIVE deployment that a baseline it
// has never seen is work it has already done.
//
// A squash replaces N migrations with one baseline describing the same end
// state. A database that applied those N is at that state, so the baseline
// must be RECORDED (or every later deploy offers it forever) and must NOT RUN
// (its full CREATE would meet tables that exist). Everything below pins one
// half of that sentence.

// squashBaseline builds a baseline that supersedes the given ids and carries
// the server-rendered bookkeeping body.
func squashBaseline(id, conn string, adoptSQL string, supersedes ...string) *applyfetchpb.Migration {
	m := mkMig(id, conn, "CREATE TABLE everything (id BIGINT PRIMARY KEY);")
	m.Supersedes = supersedes
	m.AdoptSql = adoptSQL
	return m
}

// TestRun_AdoptsBaselineTheDatabaseAlreadySatisfies is the case squash exists
// for. The database is at ts-2, which the baseline replaces — so the baseline
// must be recorded via its adopt_sql, and its own DDL must never execute.
func TestRun_AdoptsBaselineTheDatabaseAlreadySatisfies(t *testing.T) {
	const adopt = "INSERT INTO wc_migrations VALUES ('ts-9', NOW(), '\\xAB');"
	base := squashBaseline("ts-9", "main", adopt, "ts-1", "ts-2")
	dir := seedDir(t, base)
	stubA := stub.New()
	stubA.Head = "ts-2" // this DB applied the collapsed range

	var out strings.Builder
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", base.GetId(), base.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
		Out:           &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := stubA.Calls()
	if len(calls) != 1 {
		t.Fatalf("applier saw %d call(s), want exactly 1 (the adopt)", len(calls))
	}
	// The DDL must NOT have run. This is the assertion the whole feature
	// turns on: executing it would hit tables that already exist.
	if got := calls[0].GetUpSql(); got != adopt {
		t.Errorf("the applier ran:\n%s\nwant ONLY the bookkeeping body:\n%s", got, adopt)
	}
	if strings.Contains(calls[0].GetUpSql(), "CREATE TABLE everything") {
		t.Error("the baseline's DDL reached the database — adopting must record, not apply")
	}
	if !strings.Contains(out.String(), "adopted") {
		t.Errorf("output does not say the migration was adopted:\n%s", out.String())
	}
}

// TestRun_FreshDatabaseAppliesTheBaselineForReal — the mirror image, and the
// reason Adopt is keyed on the applied head rather than on the mere presence
// of a supersedes list. A database that never applied the collapsed range has
// no schema, so it needs the baseline's DDL, not a ledger row claiming work
// that never happened.
func TestRun_FreshDatabaseAppliesTheBaselineForReal(t *testing.T) {
	const adopt = "INSERT INTO wc_migrations VALUES ('ts-9', NOW(), '\\xAB');"
	base := squashBaseline("ts-9", "main", adopt, "ts-1", "ts-2")
	dir := seedDir(t, base)
	stubA := stub.New() // Head="" — fresh database

	if err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", base.GetId(), base.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := stubA.Calls()
	if len(calls) != 1 {
		t.Fatalf("applier saw %d call(s), want 1", len(calls))
	}
	if !strings.Contains(calls[0].GetUpSql(), "CREATE TABLE everything") {
		t.Errorf("a fresh database did not get the baseline's DDL; it ran:\n%s", calls[0].GetUpSql())
	}
}

// TestRun_UnrelatedHeadAppliesTheBaseline — a head that is NOT in the
// superseded set means this database's history has nothing to do with the
// collapse, so the baseline is ordinary work.
func TestRun_UnrelatedHeadAppliesTheBaseline(t *testing.T) {
	const adopt = "INSERT INTO wc_migrations VALUES ('ts-9', NOW(), '\\xAB');"
	base := squashBaseline("ts-9", "main", adopt, "ts-1", "ts-2")
	dir := seedDir(t, base)
	stubA := stub.New()
	stubA.Head = "ts-0" // sorts before the baseline, but is not superseded

	if err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", base.GetId(), base.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := stubA.Calls()
	if len(calls) != 1 || !strings.Contains(calls[0].GetUpSql(), "CREATE TABLE everything") {
		t.Errorf("an unrelated head should get the real DDL; applier ran %d call(s): %+v", len(calls), calls)
	}
}

// TestRun_BaselineWithoutAdoptSQLRefuses — fail CLOSED. A baseline this
// database has already satisfied, with no bookkeeping body to record it,
// cannot be applied (the DDL would collide) and cannot be skipped (it would
// be offered on every later deploy). Refusing names both halves; running it
// would surface as a confusing "relation already exists" from a deploy that
// was supposed to be a no-op.
func TestRun_BaselineWithoutAdoptSQLRefuses(t *testing.T) {
	base := squashBaseline("ts-9", "main", "", "ts-1", "ts-2") // no adopt_sql
	dir := seedDir(t, base)
	stubA := stub.New()
	stubA.Head = "ts-2"

	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", base.GetId(), base.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil {
		t.Fatal("want a refusal when a satisfied baseline carries no adopt_sql")
	}
	if !strings.Contains(err.Error(), "adopt_sql") {
		t.Errorf("error should name the missing piece; got: %v", err)
	}
	if len(stubA.Calls()) != 0 {
		t.Error("nothing may be executed once the refusal is decided")
	}
}

// TestPlan_MarksAdoptOnlyTheSatisfiedBaseline — Plan is where the decision is
// made, so assert it there too: the flag must not leak onto ordinary
// migrations that happen to share the connection.
func TestPlan_MarksAdoptOnlyTheSatisfiedBaseline(t *testing.T) {
	ordinary := mkMig("ts-3", "main", "ALTER TABLE x ADD col;")
	base := squashBaseline("ts-9", "main", "INSERT INTO wc_migrations VALUES ('ts-9');", "ts-1", "ts-2")
	dir := seedDir(t, ordinary, base)
	stubA := stub.New()
	stubA.Head = "ts-2"

	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", base.GetId(), base.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2 (ts-3 and the baseline)", len(pending))
	}
	for _, p := range pending {
		wantAdopt := p.Migration.GetId() == "ts-9"
		if p.Adopt != wantAdopt {
			t.Errorf("%s Adopt = %v, want %v", p.Migration.GetId(), p.Adopt, wantAdopt)
		}
	}
}

// T2-5 pass #15 (T25-A15-5, HIGH) — a MID-RANGE head is not a database the
// baseline describes, and adopting for one skips the tail's DDL forever.
//
// The adopt decision was `head != "" && supersedes(m, head)` — ANY membership.
// The justification (the baseline "describes the schema already in front of
// us") holds only when the head is the LAST id the baseline collapsed. A
// database sitting mid-range applied a PREFIX of the collapsed set, so the
// remainder of that set is exactly the DDL it still needs — and adopting
// records the baseline as done and runs none of it.
//
// It is permanent. The superseded rows never appear in a fetch again, and
// after the adopt the head is the baseline, so even resurrecting them leaves
// their ids under the cutoff. Silent, durable schema divergence on a genuine,
// console-signed artifact.
//
// The reachable sequence is ordinary: trunk pushes m031…m050 while production
// still sits at m030 (registry-ahead-of-prod is the normal state of a trunk),
// someone runs `w17ctl migrate squash --all` — the freeze reads REGISTRY live
// rows and the applied audit is contract-excluded from that reasoning — the
// baseline stamps supersedes = m001…m050 and moves the pin, and the next
// deploy adopts.
func TestPlan_RefusesAdoptFromAMidRangeHead(t *testing.T) {
	base := squashBaseline("ts-9", "main", "INSERT INTO wc_migrations VALUES ('ts-9');",
		"ts-1", "ts-2", "ts-3")
	dir := seedDir(t, base)
	stubA := stub.New()
	stubA.Head = "ts-2" // applied ts-1 and ts-2; ts-3's DDL never ran here

	_, err := migrate.Plan(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", base.GetId(), base.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil {
		t.Fatal("planned an adopt from a head in the MIDDLE of the collapsed range — the " +
			"database applied a prefix, so the rest of the range is DDL it still needs, and " +
			"adopting records the baseline while running none of it. The superseded rows are " +
			"never served again, so the gap is permanent")
	}
	if !strings.Contains(err.Error(), "ts-2") || !strings.Contains(err.Error(), "ts-3") {
		t.Errorf("the refusal must name where the database is and what it is missing; got %v", err)
	}
}

// The mirror: a head at the LAST collapsed id is the case squash exists for
// and must still adopt. Without this the fix reads as "adopt is refused",
// which would break every real squash deploy.
func TestPlan_StillAdoptsFromTheLastSupersededID(t *testing.T) {
	base := squashBaseline("ts-9", "main", "INSERT INTO wc_migrations VALUES ('ts-9');",
		"ts-1", "ts-2", "ts-3")
	dir := seedDir(t, base)
	stubA := stub.New()
	stubA.Head = "ts-3" // the whole collapsed range is applied here

	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", base.GetId(), base.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 1 || !pending[0].Adopt {
		t.Fatalf("a database at the last superseded id must still adopt; got %+v", pending)
	}
}

// A database that applied NONE of the collapsed set gets the real CREATE —
// the branch the original comment describes, kept honest.
func TestPlan_UnrelatedHeadStillDoesNotAdopt(t *testing.T) {
	base := squashBaseline("ts-9", "main", "INSERT INTO wc_migrations VALUES ('ts-9');",
		"ts-1", "ts-2")
	dir := seedDir(t, base)
	stubA := stub.New()
	stubA.Head = "" // fresh database

	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", base.GetId(), base.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 1 || pending[0].Adopt {
		t.Fatalf("a fresh database must run the baseline's DDL, not adopt it; got %+v", pending)
	}
}
