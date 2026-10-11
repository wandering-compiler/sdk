package grpcerr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/dqlbind"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// A list too long for one statement is the caller's request being too
// large: InvalidArgument with its own detail code, never Internal — whether
// the SDK refused it before building the statement or the driver did.
func TestWrap_TooManyBindValuesIsTheCallersFault(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n := dqlbind.MaxPlaceholders + 1
	args := make([]any, n)
	for i := range args {
		args[i] = i
	}
	_, sqliteErr := db.Exec(dqlbind.ExpandIn("SELECT 1 WHERE 1 IN (?)", 1, n), args...)
	if sqliteErr == nil {
		t.Fatal("the driver accepted the statement")
	}
	_, checkedErr := dqlbind.ExpandInChecked("SELECT 1 WHERE 1 IN (?)", 1, n)

	for name, tc := range map[string]struct {
		err error
		d   Dialect
	}{
		"checked expansion": {checkedErr, DialectUnknown},
		"sqlite driver":     {sqliteErr, DialectSQLite},
		"mysql driver":      {&mysql.MySQLError{Number: 1390, Message: "Prepared statement contains too many placeholders"}, DialectMySQL},
		"pgx":               {fmt.Errorf("query: %w", errors.New("extended protocol limited to 65535 parameters")), DialectPostgres},
		"lib/pq":            {errors.New("pq: got 70000 parameters but PostgreSQL only supports 65535 parameters"), DialectPostgres},
	} {
		got := Wrap(context.Background(), "T.M", tc.err, nil, tc.d)
		st := status.Convert(got)
		if st.Code() != codes.InvalidArgument {
			t.Errorf("%s: code = %s (%v), want InvalidArgument", name, st.Code(), got)
			continue
		}
		var code string
		for _, d := range st.Details() {
			if ed, ok := d.(*w17pb.ErrorDetail); ok {
				code = ed.GetCode()
			}
		}
		if code != CodeTooManyValues {
			t.Errorf("%s: detail code = %q, want %q", name, code, CodeTooManyValues)
		}
		if strings.Contains(st.Message(), "SQL variables") {
			t.Errorf("%s: the driver's text reached the status: %q", name, st.Message())
		}
	}
}
