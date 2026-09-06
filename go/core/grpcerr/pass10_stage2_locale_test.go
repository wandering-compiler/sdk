package grpcerr

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wandering-compiler/sdk/go/lib/i18n"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestWrap_LocalizesTheConstraintMessage — T2-6 pass #10, D10-5.
//
// One declaration, one catalog, two answers. The emitted Stage-1 checks
// (request-shape validation) have resolved their messages at runtime
// through `i18n.T(ctx, msgid)` since REV-149 P1.5; the constraint registry
// that answers the SAME rule when the database enforces it emitted the
// same catalog's strings as Go literals and shipped them raw. The msgid is
// extracted into the domain's `.po` alongside the Stage-1 one — the
// committed `examples/e2e-project/w17/languages/app/cs.po` carries
// `msgid "is required"` — so the translation existed and one of the two
// paths ignored it. A Czech caller who omitted a required field got Czech
// from the request-shape check and English from the NOT NULL.
//
// Since d40970b36 the message is also composed into the HEADLINE status
// text, which is what a REST gateway surfaces and what a human reads
// first, so both are asserted.
func TestWrap_LocalizesTheConstraintMessage(t *testing.T) {
	i18n.Reset()
	t.Cleanup(i18n.Reset)
	i18n.Register("cs", []byte("msgid \"is required\"\nmsgstr \"je povinné\"\n"))

	registry := &ConstraintRegistry{
		ByName: map[string]ConstraintInfo{
			"users_email_not_null": {Field: "email", Code: "REQUIRED", Message: "is required"},
		},
	}
	pgErr := &pgconn.PgError{Code: "23502", ColumnName: "email", TableName: "users", ConstraintName: "users_email_not_null"}

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("w17-language", "cs"))
	st, _ := status.FromError(Wrap(ctx, "UserMutation.CreateUser", pgErr, registry, DialectPostgres))

	details := st.Details()
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
	d, ok := details[0].(*w17pb.ErrorDetail)
	if !ok {
		t.Fatalf("detail type = %T", details[0])
	}
	if d.GetMessage() != "je povinné" {
		t.Errorf("ErrorDetail.Message = %q, want the catalog's Czech — the constraint registry ignores the locale the Stage-1 check honours", d.GetMessage())
	}
	if want := "UserMutation.CreateUser: email je povinné"; st.Message() != want {
		t.Errorf("status message = %q, want %q — the headline a REST gateway surfaces is still English", st.Message(), want)
	}
}

// TestWrap_FallsBackToTheMsgidWithNoCatalog — the other direction, and the
// reason this change is safe to make everywhere: `i18n.T` returns the bare
// msgid when nothing translates it, so a project with no `.po` reads
// exactly as it did before.
func TestWrap_FallsBackToTheMsgidWithNoCatalog(t *testing.T) {
	i18n.Reset()
	t.Cleanup(i18n.Reset)

	registry := &ConstraintRegistry{
		ByName: map[string]ConstraintInfo{
			"users_email_not_null": {Field: "email", Code: "REQUIRED", Message: "is required"},
		},
	}
	pgErr := &pgconn.PgError{Code: "23502", ColumnName: "email", TableName: "users", ConstraintName: "users_email_not_null"}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("w17-language", "cs"))
	st, _ := status.FromError(Wrap(ctx, "UserMutation.CreateUser", pgErr, registry, DialectPostgres))
	if want := "UserMutation.CreateUser: email is required"; st.Message() != want {
		t.Errorf("status message = %q, want %q", st.Message(), want)
	}
}
