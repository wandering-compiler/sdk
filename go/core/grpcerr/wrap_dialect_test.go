package grpcerr

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	"github.com/wandering-compiler/sdk/go/core/grpcerr/dialect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	_ "modernc.org/sqlite"
)

// sqliteUniqueErr produces a real modernc.org/sqlite UNIQUE
// violation against an in-memory DB — the only reliable way to
// get a driver-typed *sqlite.Error (the type is unexported-ish
// and has no public constructor).
func sqliteUniqueErr(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mustExecT(t, db, `CREATE TABLE probe_users (id INTEGER PRIMARY KEY, email TEXT, CONSTRAINT u UNIQUE(email))`)
	mustExecT(t, db, `INSERT INTO probe_users (email) VALUES ('a@b.cz')`)
	_, err = db.Exec(`INSERT INTO probe_users (email) VALUES ('a@b.cz')`)
	if err == nil {
		t.Fatal("expected UNIQUE violation")
	}
	return err
}

// sqliteCheckErr produces a real CHECK violation.
func sqliteCheckErr(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mustExecT(t, db, `CREATE TABLE chk (age INTEGER, CONSTRAINT chk_age CHECK (age >= 0))`)
	_, err = db.Exec(`INSERT INTO chk (age) VALUES (-1)`)
	if err == nil {
		t.Fatal("expected CHECK violation")
	}
	return err
}

func mustExecT(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// INVARIANT: the MySQL adapter path is reachable through Wrap — a
// recognised duplicate-entry error with a registry hit emits the
// FE-friendly structured detail.
func TestWrap_MySQL_RegistryHit(t *testing.T) {
	registry := &ConstraintRegistry{
		ByName: map[string]ConstraintInfo{
			"email_uniq": {Field: "email", Code: "UNIQUE_VIOLATION", Message: "taken"},
		},
	}
	me := &mysql.MySQLError{
		Number:  1062,
		Message: "Duplicate entry 'a@b.cz' for key 'users.email_uniq'",
	}
	var got error
	withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", me, registry, DialectMySQL)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("Code = %v, want InvalidArgument", st.Code())
	}
}

// Q47-grpc-1: a MySQL NOT_NULL error names only the column, not the table,
// so the table-keyed ByColumns lookup can't resolve it. The
// SoleNotNullByColumn degraded fallback must attribute it (→ InvalidArgument
// + structured detail) when exactly one table owns a NOT_NULL on that
// column. Without the fallback it falls through to FailedPrecondition and
// the FE-friendly detail is lost.
func TestWrap_MySQL_NotNull_SoleColumnFallback(t *testing.T) {
	registry := &ConstraintRegistry{
		SoleNotNullByColumn: map[string]ConstraintInfo{
			"email": {Field: "email", Code: "REQUIRED_VIOLATION", Message: "email is required"},
		},
	}
	me := &mysql.MySQLError{Number: 1048, Message: "Column 'email' cannot be null"}
	var got error
	withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", me, registry, DialectMySQL)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("MySQL NOT_NULL with a sole-column registry entry must map to InvalidArgument, got %v; err=%v", st.Code(), got)
	}
}

// Q47-grpc-1 safety: an ambiguous column (NOT in SoleNotNullByColumn,
// because several tables share it) must NOT be misattributed — it stays a
// FailedPrecondition rather than guessing the wrong field.
func TestWrap_MySQL_NotNull_AmbiguousColumn_NoGuess(t *testing.T) {
	registry := &ConstraintRegistry{
		SoleNotNullByColumn: map[string]ConstraintInfo{
			"email": {Field: "email", Code: "REQUIRED_VIOLATION", Message: "email is required"},
		},
	}
	me := &mysql.MySQLError{Number: 1048, Message: "Column 'name' cannot be null"} // `name` not in the index
	var got error
	withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", me, registry, DialectMySQL)
	})
	st, _ := status.FromError(got)
	if st.Code() == codes.InvalidArgument {
		t.Errorf("an ambiguous NOT_NULL column must NOT be misattributed to InvalidArgument; got %v", st.Code())
	}
}

// INVARIANT: the SQLite adapter path is reachable through Wrap — a
// UNIQUE violation resolves via the ByColumns key the adapter fills.
func TestWrap_SQLite_RegistryHit_ByColumns(t *testing.T) {
	registry := &ConstraintRegistry{
		ByColumns: map[string]ConstraintInfo{
			"probe_users:unique:email": {Field: "email", Code: "UNIQUE_VIOLATION", Message: "taken"},
		},
	}
	se := sqliteUniqueErr(t)
	var got error
	withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", se, registry, DialectSQLite)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("Code = %v, want InvalidArgument; err=%v", st.Code(), got)
	}
}

// INVARIANT: DialectUnknown has no adapter — every error falls to the
// internal-error path regardless of shape.
func TestWrap_DialectUnknown_FallsToInternal(t *testing.T) {
	me := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'k'"}
	var got error
	withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", me, &ConstraintRegistry{}, DialectUnknown)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.Internal {
		t.Errorf("Code = %v, want Internal", st.Code())
	}
}

// INVARIANT: parseByDialect dispatches per-dialect and returns
// ok=false for DialectUnknown.
func TestParseByDialect_Dispatch(t *testing.T) {
	se := sqliteCheckErr(t)
	if ce, ok := parseByDialect(se, DialectSQLite); !ok || ce.Kind != dialect.KindCheck {
		t.Errorf("SQLite dispatch: ok=%v ce=%+v", ok, ce)
	}
	me := &mysql.MySQLError{Number: 1048, Message: "Column 'name' cannot be null"}
	if ce, ok := parseByDialect(me, DialectMySQL); !ok || ce.Kind != dialect.KindNotNull {
		t.Errorf("MySQL dispatch: ok=%v ce=%+v", ok, ce)
	}
	if _, ok := parseByDialect(errors.New("x"), DialectUnknown); ok {
		t.Error("DialectUnknown must yield ok=false")
	}
}

// INVARIANT: isRetryable recognises retryable codes across the pq and
// MySQL drivers, and rejects non-retryable codes / unknown dialects.
func TestIsRetryable_AllDrivers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		d    Dialect
		want bool
	}{
		{"pq serialization", &pq.Error{Code: "40001"}, DialectPostgres, true},
		{"pq deadlock", &pq.Error{Code: "40P01"}, DialectPostgres, true},
		{"pq other", &pq.Error{Code: "23505"}, DialectPostgres, false},
		{"mysql deadlock", &mysql.MySQLError{Number: 1213}, DialectMySQL, true},
		{"mysql lock wait", &mysql.MySQLError{Number: 1205}, DialectMySQL, true},
		{"mysql other", &mysql.MySQLError{Number: 1062}, DialectMySQL, false},
		{"non-driver err pg", errors.New("boom"), DialectPostgres, false},
		{"sqlite unhandled", errors.New("busy"), DialectSQLite, false},
		{"unknown dialect", &pq.Error{Code: "40001"}, DialectUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err, tc.d); got != tc.want {
				t.Errorf("isRetryable = %v, want %v", got, tc.want)
			}
		})
	}
}

// INVARIANT: a pq.Error serialization failure routes to Aborted via
// Wrap, exercising the pq branch of isRetryable end to end.
func TestWrap_PqRetryable(t *testing.T) {
	got := Wrap(context.Background(), "M.G", &pq.Error{Code: "40001"}, nil, DialectPostgres)
	st, _ := status.FromError(got)
	if st.Code() != codes.Aborted {
		t.Errorf("Code = %v, want Aborted", st.Code())
	}
}

// INVARIANT: a MySQL deadlock routes to Aborted via Wrap.
func TestWrap_MySQLRetryable(t *testing.T) {
	got := Wrap(context.Background(), "M.G", &mysql.MySQLError{Number: 1213}, nil, DialectMySQL)
	st, _ := status.FromError(got)
	if st.Code() != codes.Aborted {
		t.Errorf("Code = %v, want Aborted", st.Code())
	}
}

// INVARIANT: a recognised constraint with table+columns but no
// registry entry surfaces FailedPrecondition naming the table/columns
// (the column-bearing branch of the unmapped detail).
func TestWrap_UnmappedConstraint_TableColumns(t *testing.T) {
	se := sqliteUniqueErr(t) // fills Table=probe_users, Columns=[email]
	var got error
	withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", se, &ConstraintRegistry{}, DialectSQLite)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("Code = %v, want FailedPrecondition", st.Code())
	}
	if !contains(st.Message(), "probe_users") || !contains(st.Message(), "email") {
		t.Errorf("message should name table+columns: %q", st.Message())
	}
}

// INVARIANT: lookupRegistry attributes a named-table FK hit via
// SoleFKByTable when the adapter fills Table (the ce.Table != ""
// FK branch).
func TestLookupRegistry_FK_NamedTable(t *testing.T) {
	registry := &ConstraintRegistry{
		SoleFKByTable: map[string]ConstraintInfo{
			"orders":   {Field: "user_id", Code: "FK_VIOLATION"},
			"comments": {Field: "post_id"},
		},
	}
	ce := &dialect.ConstraintError{Kind: dialect.KindFK, Table: "orders"}
	info, ok := lookupRegistry(registry, ce)
	if !ok || info.Field != "user_id" {
		t.Fatalf("named-table FK lookup failed: %+v ok=%v", info, ok)
	}
}

// INVARIANT: codeForKind maps every validation-class kind to
// InvalidArgument and any unknown kind to Internal.
func TestCodeForKind(t *testing.T) {
	for _, k := range []string{
		dialect.KindUnique, dialect.KindFK, dialect.KindCheck,
		dialect.KindNotNull, dialect.KindExclusion,
	} {
		if got := codeForKind(k); got != codes.InvalidArgument {
			t.Errorf("codeForKind(%q) = %v, want InvalidArgument", k, got)
		}
	}
	if got := codeForKind("mystery_kind"); got != codes.Internal {
		t.Errorf("codeForKind(unknown) = %v, want Internal", got)
	}
}
