package migrate

import (
	"context"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// The REPLACE-policy refusals (refuseReplacedHead, refuseHeadOffChain) and the
// squash adopt decision are taken by Plan against the head it read BEFORE any
// run lock existed. When another run moves the head forward while this one
// queues for the lock, the head the run then holds is not the one those
// decisions were taken against — so they are asked again, under the lock,
// against the head that is really there.
//
// Each case below plans against an empty database and finds, once the lock is
// held, a head another deploy applied in the meantime.

// chainedMigs links migs (one connection, given in id order) the way the
// console's append-only history does, and hashes each one.
func chainedMigs(migs ...*applyfetchpb.Migration) []*applyfetchpb.Migration {
	prev := ""
	for _, m := range migs {
		m.PrevContentSha256 = prev
		m.ContentSha256 = ContentHashWithChecks(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql(), "", "", m.GetChecks())
		prev = m.GetContentSha256()
	}
	return migs
}

func plainMig(id, up string) *applyfetchpb.Migration {
	return &applyfetchpb.Migration{Id: id, Connection: "main", UpSql: up, DownSql: "-- down for " + id}
}

// lockedHeadFake reports `before` until its run lock is taken and `after`
// from then on, and records every body it is asked to run.
type lockedHeadFake struct {
	covFake
	before, after string
	locked        bool
	bodies        []string
}

func (f *lockedHeadFake) AppliedHead(context.Context) (string, error) {
	if f.locked {
		return f.after, nil
	}
	return f.before, nil
}

func (f *lockedHeadFake) AcquireRunLock(context.Context) (RunLock, error) {
	f.locked = true
	return noopHeld{}, nil
}

func (f *lockedHeadFake) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	f.bodies = append(f.bodies, m.GetUpSql())
	return f.covFake.Apply(ctx, m)
}

func runAgainst(t *testing.T, f *lockedHeadFake, replaced map[string]map[string]string, migs ...*applyfetchpb.Migration) error {
	t.Helper()
	dir := seedMigs(t, migs...)
	target := migs[len(migs)-1]
	return Run(context.Background(), Config{
		Targets:       tgts(tgt("main", target.GetId(), target.GetContentSha256())),
		MigrationsDir: dir,
		ApplierFor:    func(string) (Applier, error) { return f, nil },
		Replaced:      replaced,
	})
}

// A replaced FIRST migration applied by another deploy while this run queued:
// the replacement roots the chain, so only the console's replaced list can
// tell this head from an unknown past — and it has to be asked under the lock.
func TestRun_RefusesAReplacedHeadThatAppearedWhileWaitingForTheLock(t *testing.T) {
	repl := plainMig("ts-3", "CREATE TABLE x (id BIGINT, name TEXT);")
	f := &lockedHeadFake{before: "", after: "ts-1"}
	err := runAgainst(t, f, map[string]map[string]string{"main": {"ts-1": "ts-3"}}, chainedMigs(repl)...)
	if err == nil || !strings.Contains(err.Error(), "replaced by ts-3") {
		t.Fatalf("want the replaced-head refusal under the lock, got %v", err)
	}
	if len(f.bodies) != 0 {
		t.Errorf("ran %d bod(y/ies) on top of the replaced head: %v", len(f.bodies), f.bodies)
	}
}

// A head between two chain migrations that is not on the chain (a replaced
// migration in the middle), applied by another deploy while this run queued.
func TestRun_RefusesAnOffChainHeadThatAppearedWhileWaitingForTheLock(t *testing.T) {
	migs := chainedMigs(
		plainMig("ts-1", "CREATE TABLE x (id BIGINT);"),
		plainMig("ts-3", "ALTER TABLE x ADD COLUMN name TEXT;"),
	)
	f := &lockedHeadFake{before: "ts-1", after: "ts-2"}
	err := runAgainst(t, f, nil, migs...)
	if err == nil || !strings.Contains(err.Error(), "not in the history being deployed") {
		t.Fatalf("want the off-chain refusal under the lock, got %v", err)
	}
	if len(f.bodies) != 0 {
		t.Errorf("ran %d bod(y/ies) on top of the off-chain head: %v", len(f.bodies), f.bodies)
	}
}

// A squash baseline planned for a fresh database (real DDL) meets, under the
// lock, a database another deploy brought to the end of the collapsed range:
// it must be adopted, not have its CREATE run against tables that exist.
func TestRun_AdoptsABaselineWhenTheHeadReachedItsRangeWhileWaiting(t *testing.T) {
	const adopt = "INSERT INTO w17_migrations VALUES ('ts-9');"
	base := plainMig("ts-9", "CREATE TABLE everything (id BIGINT);")
	base.Supersedes = []string{"ts-1", "ts-2"}
	base.AdoptSql = adopt
	f := &lockedHeadFake{before: "", after: "ts-2"}
	if err := runAgainst(t, f, nil, chainedMigs(base)...); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.bodies) != 1 || f.bodies[0] != adopt {
		t.Fatalf("ran %q, want only the adopt body %q", f.bodies, adopt)
	}
}

// …and a database another deploy left MID-range is refused, as Plan refuses it.
func TestRun_RefusesABaselineWhenTheHeadStoppedMidRangeWhileWaiting(t *testing.T) {
	base := plainMig("ts-9", "CREATE TABLE everything (id BIGINT);")
	base.Supersedes = []string{"ts-1", "ts-2"}
	base.AdoptSql = "INSERT INTO w17_migrations VALUES ('ts-9');"
	f := &lockedHeadFake{before: "", after: "ts-1"}
	err := runAgainst(t, f, nil, chainedMigs(base)...)
	if err == nil || !strings.Contains(err.Error(), "refusing to adopt squash baseline") {
		t.Fatalf("want the mid-range adopt refusal under the lock, got %v", err)
	}
	if len(f.bodies) != 0 {
		t.Errorf("ran %d bod(y/ies): %v", len(f.bodies), f.bodies)
	}
}
