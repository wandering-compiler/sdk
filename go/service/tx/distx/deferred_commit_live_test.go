package distx_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// A foreign key checked at COMMIT ((w17.db.column).deferrable) that a
// statement of the transaction left broken fails at the COMMIT — the
// statement itself succeeded. The commit maps it as a statement-level
// violation is mapped (InvalidArgument with a detail), not as an internal
// fault carrying the driver's message.
func TestCommit_ADeferredKeyStillBrokenIsAConstraintViolation_Live(t *testing.T) {
	db := openLivePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, c := fmt.Sprintf("dfp_%d", time.Now().UnixNano()), fmt.Sprintf("dfc_%d", time.Now().UnixNano())
	for _, stmt := range []string{
		"CREATE TABLE " + p + " (id BIGINT PRIMARY KEY)",
		"CREATE TABLE " + c + " (id BIGINT PRIMARY KEY, p_id BIGINT CONSTRAINT " + c + "_p_fkey REFERENCES " + p + " (id) DEFERRABLE INITIALLY DEFERRED)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE "+c+", "+p) })

	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	srv := distx.NewServer(reg)
	resp, err := srv.Begin(ctx, &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx, release, err := reg.LookupTx(ctx, resp.GetTxId(), "main")
	if err != nil {
		t.Fatalf("LookupTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+c+" (id, p_id) VALUES (1, 42)"); err != nil {
		t.Fatalf("the insert must pass — the key is checked at COMMIT: %v", err)
	}
	release()

	_, err = srv.Commit(ctx, &distxpb.CommitRequest{TxId: resp.GetTxId()})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("commit = %v, want InvalidArgument", err)
	}
	var code string
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			code = ed.GetCode()
		}
	}
	if code != grpcerr.CodeConstraintAtCommit {
		t.Errorf("detail code = %q, want %q (status %v)", code, grpcerr.CodeConstraintAtCommit, err)
	}
}
