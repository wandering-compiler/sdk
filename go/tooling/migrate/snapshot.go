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

// SnapshotterFor produces a Snapshotter for a connection name — the
// snapshot-tier analogue of ApplierFor. The branch-switch reconcile
// calls it once per stateful connection it dumps or restores.
// Returning an error aborts the reconcile rather than silently
// skipping a store (a half-snapshotted branch is worse than a loud
// refusal, mirroring ApplierFor's posture).
type SnapshotterFor func(connectionName string) (Snapshotter, error)
