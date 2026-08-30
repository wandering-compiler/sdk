package migrate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// T2-5 B11-1 — the signed lock pinned only the TARGET migration.
//
// Every migration applied on the way to that target carried nothing but its
// own self-hash, which whoever holds the artifact directory recomputes for
// free. Under this project's documented deploy split (CI bakes the fetch
// output; the deploy host applies offline against local files) that directory
// really travels, so the finding was measured live rather than argued: a lock
// pinning target 0003 with the correct hash, `up_sql` rewritten in the
// INTERMEDIATE 0002 and its own content_sha256 recomputed, produced
// `applied: [0001, 0002-with-the-attacker's-GRANT, 0003]` and exit 0.
//
// The fix chains each migration's hash into its successor's, so the one hash
// the lock pins covers the whole history, and apply selects what to run by
// WALKING that chain instead of by id range. This file is that measurement,
// kept as a test.

// rewriteOnDisk edits a migration artifact in place the way an attacker
// holding the baked directory would: change the body, then recompute the
// file's own content_sha256 so the per-file integrity check still passes.
// Everything here is available to anyone with write access to the directory —
// no key, no console.
func rewriteOnDisk(t *testing.T, dir, conn, id, newUpSQL string) {
	t.Helper()
	path := filepath.Join(dir, conn, id+".json")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := &applyfetchpb.Migration{}
	if err := protojson.Unmarshal(buf, m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	m.UpSql = newUpSQL
	m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql(), "")
	out, err := protojson.MarshalOptions{UseProtoNames: true, Multiline: true, Indent: "  "}.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// planErr runs Plan against dir with the given lock pin and returns the error
// (nil on success) plus the pending ids.
func planErr(t *testing.T, dir string, tgt lockTarget, head string) ([]string, error) {
	t.Helper()
	stubA := stub.New()
	stubA.Head = head
	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets:       targets(tgt),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.Migration.GetId())
	}
	return ids, nil
}

// The finding, reproduced and closed. The attacker rewrites the intermediate
// and recomputes its self-hash — the only per-file check that existed — while
// the lock's pinned TARGET hash is left untouched and still correct.
func TestPlan_TamperedIntermediateIsRefused(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "CREATE TABLE b();")
	m3 := mkMig("ts-3", "main", "CREATE TABLE c();")
	dir := seedDir(t, m1, m2, m3)
	pin := lockTarget{"main", m3.GetId(), m3.GetContentSha256()}

	// Sanity: the untampered set plans all three. Without this the refusal
	// below could be any old failure and the test would still "pass".
	ids, err := planErr(t, dir, pin, "")
	if err != nil {
		t.Fatalf("clean set must plan: %v", err)
	}
	if strings.Join(ids, ",") != "ts-1,ts-2,ts-3" {
		t.Fatalf("clean set planned %v, want all three in order", ids)
	}

	rewriteOnDisk(t, dir, "main", "ts-2", "CREATE TABLE b(); GRANT ALL ON ALL TABLES IN SCHEMA public TO attacker;")

	ids, err = planErr(t, dir, pin, "")
	if err == nil {
		t.Fatalf("a tampered intermediate must be refused; Plan returned %v", ids)
	}
	// The refusal must be the CHAIN's, not an incidental parse failure — the
	// attacker kept every per-file check satisfied, so anything else means
	// this test is measuring the wrong thing.
	if !strings.Contains(err.Error(), "chains to predecessor") {
		t.Errorf("refusal should name the broken chain link, got: %v", err)
	}
}

// The other half of selecting by chain rather than by id range: a migration
// INSERTED into the range is not on the chain, so it never runs. The old
// filter would have executed it — it sorts between two legitimate ids and
// hashes to its own body correctly.
func TestPlan_InsertedMigrationIsNotOnTheChain(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m3 := mkMig("ts-3", "main", "CREATE TABLE c();")
	dir := seedDir(t, m1, m3)
	pin := lockTarget{"main", m3.GetId(), m3.GetContentSha256()}

	// Forged, self-consistent, and squarely inside the applied range.
	inserted := mkMig("ts-2", "main", "GRANT ALL ON ALL TABLES IN SCHEMA public TO attacker;")
	inserted.PrevContentSha256 = m1.GetContentSha256()
	inserted.ContentSha256 = migrate.ContentHash(inserted.GetUpSql(), "", "", inserted.GetDownSql(), inserted.GetPrevContentSha256(), nil, "", "")
	if err := migrate.WriteMigration(dir, inserted); err != nil {
		t.Fatalf("seed the inserted migration: %v", err)
	}

	ids, err := planErr(t, dir, pin, "")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if strings.Join(ids, ",") != "ts-1,ts-3" {
		t.Errorf("planned %v — a migration no link points at must not be applied", ids)
	}
}

// A predecessor that is not in the fetched set is a history the walk cannot
// prove. Refuse rather than apply the provable tail: the tail is not the set
// the operator asked for.
func TestPlan_MissingPredecessorIsRefused(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "CREATE TABLE b();")
	dir := seedDir(t, m1, m2)
	pin := lockTarget{"main", m2.GetId(), m2.GetContentSha256()}

	if err := os.Remove(filepath.Join(dir, "main", "ts-1.json")); err != nil {
		t.Fatalf("remove predecessor: %v", err)
	}
	if _, err := planErr(t, dir, pin, ""); err == nil {
		t.Fatal("a chain with a missing predecessor must be refused")
	} else if !strings.Contains(err.Error(), "migrate fetch") {
		t.Errorf("refusal should name the fix; got: %v", err)
	}
}

// Two artifacts carrying the same content hash make the link ambiguous.
// Choosing either one is choosing on the attacker's behalf.
func TestPlan_DuplicateContentHashIsRefused(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "CREATE TABLE b();")
	dir := seedDir(t, m1, m2)
	pin := lockTarget{"main", m2.GetId(), m2.GetContentSha256()}

	// A byte-identical copy of ts-1 under a different id.
	dup := &applyfetchpb.Migration{
		Id: "ts-1b", Connection: "main",
		UpSql: m1.GetUpSql(), DownSql: m1.GetDownSql(),
		PrevContentSha256: m1.GetPrevContentSha256(),
		ContentSha256:     m1.GetContentSha256(),
	}
	if err := migrate.WriteMigration(dir, dup); err != nil {
		t.Fatalf("seed duplicate: %v", err)
	}
	if _, err := planErr(t, dir, pin, ""); err == nil {
		t.Fatal("two artifacts with one content hash must be refused")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("refusal should name the ambiguity; got: %v", err)
	}
}

// Artifacts produced before chaining have no links at all. Applying them by
// id range is the defect itself, and it would be reachable by simply
// stripping prev from the files — so there is no fallback. Refuse, and name
// the one command that fixes it.
func TestPlan_UnchainedArtifactSetIsRefused(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "CREATE TABLE b();")
	dir := seedDirUnchained(t, m1, m2) // no links, the pre-fix shape
	pin := lockTarget{"main", m2.GetId(), m2.GetContentSha256()}

	if _, err := planErr(t, dir, pin, ""); err == nil {
		t.Fatal("an unchained multi-migration set must be refused")
	} else if !strings.Contains(err.Error(), "migrate fetch") {
		t.Errorf("refusal should name the fix; got: %v", err)
	}
}

// A chain root is legitimate on its own — a fresh project's first migration,
// or a squash baseline that replaced everything behind it. The refusal above
// must not fire here, or every new project stops deploying.
func TestPlan_LoneRootTargetIsAllowed(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	dir := seedDir(t, m1)
	if m1.GetPrevContentSha256() != "" {
		t.Fatalf("fixture is not a root: prev = %q", m1.GetPrevContentSha256())
	}
	ids, err := planErr(t, dir, lockTarget{"main", m1.GetId(), m1.GetContentSha256()}, "")
	if err != nil {
		t.Fatalf("a lone root must plan: %v", err)
	}
	if strings.Join(ids, ",") != "ts-1" {
		t.Errorf("planned %v, want [ts-1]", ids)
	}
}

// A database mid-history still gets only what it is missing — the chain
// decides WHICH migrations are legitimate, the applied head decides which of
// them still have to run. Conflating the two would re-apply history.
func TestPlan_ChainRespectsTheAppliedHead(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "CREATE TABLE b();")
	m3 := mkMig("ts-3", "main", "CREATE TABLE c();")
	dir := seedDir(t, m1, m2, m3)

	ids, err := planErr(t, dir, lockTarget{"main", m3.GetId(), m3.GetContentSha256()}, "ts-1")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if strings.Join(ids, ",") != "ts-2,ts-3" {
		t.Errorf("planned %v, want [ts-2 ts-3]", ids)
	}
}

// The chain is per connection: two connections' histories are independent
// walks, and a link must never resolve across them.
func TestPlan_ChainsAreIndependentPerConnection(t *testing.T) {
	a1 := mkMig("ts-1", "alpha", "CREATE TABLE a1();")
	a2 := mkMig("ts-2", "alpha", "CREATE TABLE a2();")
	b1 := mkMig("ts-1", "beta", "CREATE TABLE b1();")
	dir := seedDir(t, a1, a2, b1)

	if b1.GetPrevContentSha256() != "" {
		t.Errorf("beta's first migration must root its own chain, got prev = %q", b1.GetPrevContentSha256())
	}

	stubA := stub.New()
	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: targets(
			lockTarget{"alpha", a2.GetId(), a2.GetContentSha256()},
			lockTarget{"beta", b1.GetId(), b1.GetContentSha256()},
		),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want 3 (two on alpha, one on beta)", len(pending))
	}
}
