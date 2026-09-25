package migrate

import (
	"context"
	"io"
)

// Snapshotter is the per-connection dump/restore surface for the
// dev DB lifecycle (`docs/specs/storage/dev-db-lifecycle.md`). It is
// a deliberate mirror of Applier: one impl per target type
// (PG / MySQL / SQLite / Redis / NATS / S3), selected by DSN scheme
// through the same factory, but it serves the orthogonal concern of
// branch-scoped snapshots rather than migration apply.
//
// A snapshot is a full dump of a store's stateful contents to an
// opaque byte stream that Restore can replay into a (possibly wiped)
// store of the same dialect. Snapshots are disposable dev scratch
// (`w17/tmp/<branch>/db/`), not a backup/DR mechanism — see the
// spec's Non-goals.
//
// Atomicity is NOT required per store: the branch-switch reconcile
// quiesces writers (stops every container except the stateful
// stores) before dumping, so a plain read-dump is consistent. Impls
// use an atomic mechanism where the store offers one (pg_dump's
// snapshot) and a manual read-dump otherwise.
type Snapshotter interface {
	// Dump writes the store's full stateful contents to w as an
	// opaque, dialect-specific byte stream. The only contract on the
	// bytes is that the same impl's Restore can replay them. Errors
	// surface verbatim with a short dialect prefix so snapshot logs
	// cluster by store.
	Dump(ctx context.Context, w io.Writer) error

	// Restore replays a stream previously produced by Dump (same
	// dialect) into the target store, overwriting existing state. It
	// is the inverse of Dump and must be idempotent against a store
	// that already holds the snapshot's objects (dev restore runs
	// against both freshly-wiped and partially-populated stores).
	Restore(ctx context.Context, r io.Reader) error
}

// ObjectCounter is an OPTIONAL capability a Snapshotter may also offer: it
// answers whether the store currently holds anything a snapshot would carry.
//
// It exists to separate two states a dump cannot tell apart. An object-less SQL
// dump means either "this store is empty" or "the dump reached a different
// database than the one this store names" — and the second is how a branch
// switch came to restore a 722-byte file over a live database (marb #68). The
// caller used to infer the second from the first and refuse both, which made a
// brand-new empty store unswitchable except through the flag that WIPES.
//
// Optional on purpose: the answer is dialect-specific and only the SQL
// dialects are asked the question at all. A Snapshotter that does not implement
// this leaves the caller with the inference, so a missing capability keeps the
// safe behaviour rather than granting the permissive one.
type ObjectCounter interface {
	// StoreObjects names the stateful objects the store holds — tables,
	// sequences, views — outside the engine's own system schemas and outside
	// anything an EXTENSION owns, which is the set a dump carries.
	//
	// Names rather than a count, because the caller's refusal has to be
	// diagnosable: "the store holds tables and the dump carries none" is a
	// claim about two tools disagreeing, and the only way to tell a misdirected
	// dump from a mismatch in what each considers an object is to say WHICH
	// objects were counted. Guessing that from a bare number is how a message
	// comes to name the wrong cause.
	//
	// An error means the question could not be answered, which is NOT an empty
	// store: the caller must not read a failure as "holds nothing".
	StoreObjects(ctx context.Context) ([]string, error)
}

// ObjectCountSQLPostgres / ObjectCountSQLMySQL are the queries behind
// [ObjectCounter], in the public package because TWO callers need the same
// words: the dialect Snapshotter, which runs them through the host's client, and
// w17ctl's in-container route, which runs them through `docker exec`. Two
// spellings of "what counts as an object" would be free to drift, and the drift
// would show up as a snapshot accepted on one route and refused on the other —
// the harder bug to see, because each route is self-consistent.
//
// Postgres counts ordinary and partitioned tables, sequences, views,
// materialized views and foreign tables outside the system schemas: the same
// vocabulary the caller's own object check reads out of a dump. MySQL's
// information_schema.tables covers base tables and views, which is the whole of
// what mysqldump writes CREATE statements for.
const (
	// ⚠️ Extension-owned relations are excluded, and that exclusion is the
	// whole difficulty. `pg_dump` does not emit them — the extension recreates
	// them on CREATE EXTENSION — so a store holding nothing but, say,
	// pg_stat_statements' views counts as EMPTY here and dumps empty there.
	// Counting them made the two tools disagree about a store neither thought
	// held anything, and the caller reported a misdirected dump.
	ObjectCountSQLPostgres = `SELECT n.nspname || '.' || c.relname FROM pg_class c ` +
		`JOIN pg_namespace n ON n.oid = c.relnamespace ` +
		`WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') ` +
		`AND n.nspname NOT LIKE 'pg\_toast%' ` +
		`AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f') ` +
		`AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid ` +
		`AND d.classid = 'pg_class'::regclass AND d.deptype = 'e') ` +
		`ORDER BY 1`

	ObjectCountSQLMySQL = "SELECT CONCAT(table_schema, '.', table_name) FROM information_schema.tables " +
		"WHERE table_schema = DATABASE() ORDER BY 1"
)

// SnapshotterFor produces a Snapshotter for a connection name — the
// snapshot-tier analogue of ApplierFor. The branch-switch reconcile
// calls it once per stateful connection it dumps or restores.
// Returning an error aborts the reconcile rather than silently
// skipping a store (a half-snapshotted branch is worse than a loud
// refusal, mirroring ApplierFor's posture).
type SnapshotterFor func(connectionName string) (Snapshotter, error)
