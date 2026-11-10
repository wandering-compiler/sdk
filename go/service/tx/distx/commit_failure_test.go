package distx_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// commitFailDriver is a database/sql driver whose COMMIT fails with the
// error the DSN names: what PostgreSQL or MySQL answer when a transaction
// cannot be serialized, or deadlocks, at its COMMIT.
type commitFailDriver struct{}
type commitFailConn struct{ err error }
type commitFailTx struct{ err error }

var commitFailures = map[string]error{
	"pg-40001":    &pgconn.PgError{Code: "40001", Severity: "ERROR", Message: "could not serialize access due to read/write dependencies among transactions"},
	"pg-40P01":    &pgconn.PgError{Code: "40P01", Severity: "ERROR", Message: "deadlock detected"},
	"mysql-1213":  &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"},
	"pg-other-xx": &pgconn.PgError{Code: "XX000", Severity: "ERROR", Message: "internal error"},
}

func (commitFailDriver) Open(dsn string) (driver.Conn, error) {
	return commitFailConn{err: commitFailures[dsn]}, nil
}
func (commitFailConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no statements") }
func (commitFailConn) Close() error                        { return nil }
func (c commitFailConn) Begin() (driver.Tx, error)         { return commitFailTx(c), nil }
func (t commitFailTx) Commit() error                       { return t.err }
func (commitFailTx) Rollback() error                       { return nil }

func init() { sql.Register("distx-commit-fail", commitFailDriver{}) }

// newServerOn serves a coordinator over one database over bufconn.
func newServerOn(t *testing.T, db *sql.DB) (distxpb.W17DistributedTransactionClient, *txregistry.Memory) {
	t.Helper()
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	srv := grpc.NewServer()
	distxpb.RegisterW17DistributedTransactionServer(srv, distx.NewServer(reg))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough://bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return distxpb.NewW17DistributedTransactionClient(conn), reg
}

// A serialization failure or a deadlock raised at COMMIT is what the same
// failure at a statement is: Aborted with CONFLICT_RETRY, the caller's signal
// to run the whole transaction again — not an Internal fault. Any other
// COMMIT failure stays Internal.
func TestServer_Commit_ASerializationFailureAtCommitIsAborted(t *testing.T) {
	for dsn, want := range map[string]codes.Code{
		"pg-40001":    codes.Aborted,
		"pg-40P01":    codes.Aborted,
		"mysql-1213":  codes.Aborted,
		"pg-other-xx": codes.Internal,
	} {
		t.Run(dsn, func(t *testing.T) {
			db, err := sql.Open("distx-commit-fail", dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			cl, _ := newServerOn(t, db)
			b, err := cl.Begin(context.Background(), &distxpb.BeginRequest{ConnectionName: "main"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = cl.Commit(context.Background(), &distxpb.CommitRequest{TxId: b.GetTxId()})
			st, _ := status.FromError(err)
			if st.Code() != want {
				t.Fatalf("COMMIT failing with %s answered %v %q, want %v", dsn, st.Code(), st.Message(), want)
			}
			var code string
			for _, d := range st.Details() {
				if ed, ok := d.(*w17pb.ErrorDetail); ok {
					code = ed.GetCode()
				}
			}
			if want == codes.Aborted && code != grpcerr.CodeConflictRetry {
				t.Fatalf("detail code = %q, want %q", code, grpcerr.CodeConflictRetry)
			}
			// Not committed: the transaction is gone, a second Commit is NotFound.
			_, err = cl.Commit(context.Background(), &distxpb.CommitRequest{TxId: b.GetTxId()})
			if status.Code(err) != codes.NotFound {
				t.Fatalf("second commit = %v, want NotFound", err)
			}
		})
	}
}
