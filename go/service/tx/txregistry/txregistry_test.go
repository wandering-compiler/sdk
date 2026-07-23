package txregistry_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"google.golang.org/grpc/metadata"
	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// fakeRegistry maps tx ids to (tx, connName) pairs. Tests
// pre-populate it + assert AdoptTx threads through.
type fakeRegistry map[string]fakeEntry

type fakeEntry struct {
	tx       *sql.Tx
	connName string
}

func (f fakeRegistry) LookupTx(id, connName string) (*sql.Tx, error) {
	entry, ok := f[id]
	if !ok {
		return nil, txregistry.ErrUnknownTxID
	}
	if entry.connName != connName {
		return nil, txregistry.ErrConnectionMismatch
	}
	return entry.tx, nil
}

// TestAdoptTx_NilRegistry — nil registry short-circuits without
// touching the metadata. Important: the handler scaffolding
// passes s.txRegistry which is nil in the M1 default; we must
// not panic on the metadata read.
func TestAdoptTx_NilRegistry(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, "tx-1"))
	tx, ok, err := txregistry.AdoptTx(ctx, nil, "main")
	if ok || tx != nil || err != nil {
		t.Errorf("AdoptTx(nil registry) = (%v, %v, %v); want (nil, false, nil)", tx, ok, err)
	}
}

// TestAdoptTx_NoMetadata — context without any incoming
// metadata = caller didn't pass headers. Fresh-tx path.
func TestAdoptTx_NoMetadata(t *testing.T) {
	reg := fakeRegistry{}
	tx, ok, err := txregistry.AdoptTx(context.Background(), reg, "main")
	if ok || tx != nil || err != nil {
		t.Errorf("AdoptTx(no metadata) = (%v, %v, %v); want (nil, false, nil)", tx, ok, err)
	}
}

// TestAdoptTx_HeaderAbsent — metadata present but no
// `w17-tx-id` key. Fresh-tx path.
func TestAdoptTx_HeaderAbsent(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("unrelated-header", "x"))
	reg := fakeRegistry{}
	tx, ok, err := txregistry.AdoptTx(ctx, reg, "main")
	if ok || tx != nil || err != nil {
		t.Errorf("AdoptTx(no header) = (%v, %v, %v); want (nil, false, nil)", tx, ok, err)
	}
}

// TestAdoptTx_HeaderEmpty — `w17-tx-id` is present but the
// value is the empty string. Defensive: caller-mistake path
// shouldn't hit the registry with "".
func TestAdoptTx_HeaderEmpty(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, ""))
	reg := fakeRegistry{}
	tx, ok, err := txregistry.AdoptTx(ctx, reg, "main")
	if ok || tx != nil || err != nil {
		t.Errorf("AdoptTx(empty header) = (%v, %v, %v); want (nil, false, nil)", tx, ok, err)
	}
}

// TestAdoptTx_UnknownID — header carries an id the registry
// doesn't know. Fresh-tx path; the generator can't distinguish
// "caller never passed an id" from "caller passed an
// unrecognised id" — both reduce to ok=false here. err stays
// nil because ErrUnknownTxID is the documented fall-through
// signal.
func TestAdoptTx_UnknownID(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, "tx-unknown"))
	reg := fakeRegistry{}
	tx, ok, err := txregistry.AdoptTx(ctx, reg, "main")
	if ok || tx != nil || err != nil {
		t.Errorf("AdoptTx(unknown id) = (%v, %v, %v); want (nil, false, nil)", tx, ok, err)
	}
}

// TestAdoptTx_ConnectionMismatch — header carries a known id
// but the recorded connection is different from the calling
// method's. Returns (nil, false, ErrConnectionMismatch); the
// generator surfaces this as `codes.InvalidArgument` so the
// caller sees a clear cross-connection diagnostic.
func TestAdoptTx_ConnectionMismatch(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	openTx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer openTx.Rollback()

	reg := fakeRegistry{"tx-42": {tx: openTx, connName: "audit"}}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, "tx-42"))
	tx, ok, err := txregistry.AdoptTx(ctx, reg, "main")
	if ok {
		t.Error("expected ok=false on connection mismatch")
	}
	if tx != nil {
		t.Errorf("expected nil tx on mismatch, got %v", tx)
	}
	if !errors.Is(err, txregistry.ErrConnectionMismatch) {
		t.Errorf("expected ErrConnectionMismatch, got %v", err)
	}
}

// TestAdoptTx_HappyPath — header present + registry resolves
// the id on the matching connection → returns the live tx.
func TestAdoptTx_HappyPath(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	openTx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer openTx.Rollback()

	reg := fakeRegistry{"tx-42": {tx: openTx, connName: "main"}}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, "tx-42"))
	got, ok, err := txregistry.AdoptTx(ctx, reg, "main")
	if !ok {
		t.Fatal("expected ok=true; got false")
	}
	if err != nil {
		t.Errorf("expected nil err on happy path, got %v", err)
	}
	if got != openTx {
		t.Errorf("AdoptTx returned a different *sql.Tx than the registry held")
	}
}
