package postgres

import (
	"context"
	"reflect"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// TestSplitStatements pins the naive `;`-boundary splitter contract:
// whitespace is trimmed, empties (including blank lines and bare
// trailing semicolons) are dropped, and each emitted statement
// re-acquires its trailing `;`.
func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \n\t ", nil},
		{"single no trailing semi", "SELECT 1", []string{"SELECT 1;"}},
		{"single trailing semi", "SELECT 1;", []string{"SELECT 1;"}},
		{"multiple", "A; B; C", []string{"A;", "B;", "C;"}},
		{"trailing empty dropped", "A;;", []string{"A;"}},
		{"interior blank dropped", "A;\n\n;B", []string{"A;", "B;"}},
		{"surrounding whitespace trimmed", "  A  ;  B  ", []string{"A;", "B;"}},
		// writer-F5 — a semicolon inside a single-quoted literal is NOT a boundary.
		{"semicolon in string literal", "CREATE INDEX CONCURRENTLY i ON t (c) WHERE s = 'a;b'; UPDATE t SET x = 1", []string{"CREATE INDEX CONCURRENTLY i ON t (c) WHERE s = 'a;b';", "UPDATE t SET x = 1;"}},
		{"escaped quote in literal", "INSERT INTO t VALUES ('a''b;c'); SELECT 1", []string{"INSERT INTO t VALUES ('a''b;c');", "SELECT 1;"}},
		{"semicolon in line comment", "SELECT 1 -- a;b\nWHERE x = 1; SELECT 2", []string{"SELECT 1 -- a;b\nWHERE x = 1;", "SELECT 2;"}},
		{"semicolon in block comment", "SELECT 1 /* a;b */; SELECT 2", []string{"SELECT 1 /* a;b */;", "SELECT 2;"}},
		{"semicolon in dollar-quote", "DO $$ BEGIN x; END $$; SELECT 2", []string{"DO $$ BEGIN x; END $$;", "SELECT 2;"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitStatements(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitStatements(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

// Q65-engine-1: a migration with NO post-tx skirt must return early from
// applyPostTx — before touching the connection — so the common no-skirt
// migration never pays the advisory-lock round-trip. Constructed with a
// nil conn so any conn use would panic, proving the early return fires.
func TestApplyPostTx_EmptySkirtSkipsLock(t *testing.T) {
	a := &Applier{} // conn == nil
	m := &applyfetchpb.Migration{UpPostTx: ""}
	if err := a.applyPostTx(context.Background(), m); err != nil {
		t.Fatalf("empty-skirt applyPostTx = %v, want nil (must not touch conn)", err)
	}
	// Whitespace-only skirt splits to zero statements — same fast path.
	if err := a.applyPostTx(context.Background(), &applyfetchpb.Migration{UpPostTx: "  \n ;; \t"}); err != nil {
		t.Fatalf("whitespace-only-skirt applyPostTx = %v, want nil", err)
	}
}

// TestClose_NilConnIsNoop pins the idempotency guard: Close on an
// Applier whose conn is already nil returns nil and never panics.
// Constructed directly because the only public constructor (New)
// always dials a live conn.
func TestClose_NilConnIsNoop(t *testing.T) {
	a := &Applier{}
	if err := a.Close(); err != nil {
		t.Errorf("Close on nil-conn Applier = %v, want nil", err)
	}
}
