package mysql

import (
	"strings"
	"testing"
)

// TestRunLockName_CarriesTheSchema — T3-7 pass #15, `A15-2`. A defect in
// pass #14's own fix, one pass later, and the same shape that pass spent
// itself on: a comment describing a design the code does not implement.
//
// `runLockName`'s comment says "MySQL's GET_LOCK namespace is per-SERVER,
// not per-schema, so the name carries the schema to keep two databases on
// one instance independent — which the shadow-DB matrix relies on". The
// constant it sat above was the bare string `w17_migrate_apply`, carrying
// no schema at all. The reasoning was right and never reached the code.
//
// The consequence is not a missed lock but a FALSE one: two different
// databases on one server contend for the same name, so a perfectly
// legitimate apply aborts with "another migration run is in progress
// against this target" naming a run that is against a different database
// entirely. On the shadow-DB matrix — one instance per (dialect, version),
// every project a database inside it — that is the ordinary case, not an
// edge.
//
// The Postgres sibling this was modelled on is safe only because PG
// advisory locks are per-DATABASE. Copying its shape without its namespace
// is what produced the gap.
func TestRunLockName_CarriesTheSchema(t *testing.T) {
	a := runLockNameFor("orders_shadow")
	b := runLockNameFor("billing_shadow")

	if a == b {
		t.Fatalf("two databases on one server share the lock name %q — GET_LOCK is "+
			"per-SERVER, so one project's apply aborts another's with a message naming "+
			"a concurrent run that is against a different database entirely", a)
	}
	// ⚠️ EVERY property on EVERY input. The first version of this test
	// checked the length only on the short names and distinguishability only
	// on the long ones — so the two assertions never met, and the hashed
	// branch (the only one that can overrun) was never length-checked. It
	// produced 65 characters, which MySQL REFUSES with error 4163, and the
	// test was green (T3-7 pass #15, B15-3's residue).
	long1 := runLockNameFor(strings.Repeat("a", 80) + "_one")
	long2 := runLockNameFor(strings.Repeat("a", 80) + "_two")

	for _, name := range []string{a, b, long1, long2} {
		if !strings.HasPrefix(name, "w17_migrate_apply") {
			t.Errorf("lock name %q lost its prefix — the name is what an operator sees in "+
				"`performance_schema.metadata_locks`, so it has to stay recognisable", name)
		}
		// MySQL truncates a GET_LOCK name over 64 characters, and two names
		// that truncate together are the collision this test exists to
		// prevent — reintroduced by the fix for it.
		if len(name) > mysqlMaxLockName {
			t.Errorf("lock name %q is %d chars; MySQL REFUSES anything over %d with "+
				"ER_USER_LOCK_WRONG_NAME (4163) — measured on mysql84 — so every apply "+
				"against a schema this long fails at the lock, before any DDL runs",
				name, len(name), mysqlMaxLockName)
		}
	}

	// A long schema name must still be distinguishable from another long one
	// that shares its prefix — the TenantRole lesson from pass #13, one
	// package over.
	if long1 == long2 {
		t.Errorf("two long schema names collapse to one lock name:\n  %s\n  %s", long1, long2)
	}
}
