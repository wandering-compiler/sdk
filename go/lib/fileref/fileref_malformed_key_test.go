package fileref_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/fileref"
)

// strictQuerier answers like a Postgres or MySQL text column does when it
// is handed bytes no UTF-8 column can hold: an error, not "no row". It
// counts the queries, so a test sees whether the key reached the driver.
type strictQuerier struct {
	db      *sql.DB
	queries int
}

func (q *strictQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.queries++
	for _, a := range args {
		if s, ok := a.(string); ok && (!utf8.ValidString(s) || strings.ContainsRune(s, 0)) {
			// A row whose Scan fails with the driver's error: a query
			// against a closed database reports it on Scan.
			return q.db.QueryRowContext(ctx, "SELECT 1")
		}
	}
	return q.db.QueryRowContext(ctx, query, args...)
}

// TestReadable_AMalformedKeyIsNoFile: a key no stored object can have (not
// UTF-8, a NUL, a control character) is a file nobody may read — false,
// not a storage error. The download route turns any error into a 500 and
// an error-log line, so on a Postgres or MySQL tier any authenticated
// caller could produce those at will with `GET /files/<bucket>/%ff`.
func TestReadable_AMalformedKeyIsNoFile(t *testing.T) {
	closed, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close() // every query on it fails, like the encoding error does
	q := &strictQuerier{db: closed}
	srv := fileref.NewServer(fileref.Ref{
		Bucket: "attachments", DB: q,
		Query: `SELECT 1 FROM notes WHERE file = ? LIMIT 1`,
		Args:  []fileref.Arg{fileref.KeyArg()},
	})
	for _, key := range []string{
		"attachments/\xff",
		"attachments/a\x00b",
		"attachments/a\nb",
		"attachments/a\x7fb",
	} {
		got, err := srv.Readable(context.Background(), wrapperspb.String(key))
		if err != nil {
			t.Errorf("Readable(%q): %v, want false without an error", key, err)
			continue
		}
		if got.GetValue() {
			t.Errorf("Readable(%q) = true", key)
		}
	}
	if q.queries != 0 {
		t.Errorf("a malformed key reached the database %d times", q.queries)
	}
}

// TestReadableClient_AMalformedKeyIsNoFile: the gateway side answers a
// malformed key itself. gRPC cannot marshal a string that is not UTF-8, so
// asking would fail as a 500 before storage ever saw the key.
func TestReadableClient_AMalformedKeyIsNoFile(t *testing.T) {
	tier := &fakeTier{readable: map[string]bool{}}
	for _, key := range []string{"", "attachments/\xff", "attachments/a\x00b"} {
		got, err := fileref.Readable(context.Background(), tier, key)
		if err != nil || got {
			t.Errorf("Readable(%q) = %v, %v; want false, nil", key, got, err)
		}
	}
	if len(tier.asked) != 0 {
		t.Errorf("a malformed key was sent to storage: %q", tier.asked)
	}
}
