package grpcerr

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wandering-compiler/sdk/go/core/grpcerr/dialect"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
)

func TestWrap_Nil(t *testing.T) {
	if got := Wrap(context.Background(), "M", nil, nil, DialectPostgres); got != nil {
		t.Errorf("nil err should yield nil, got %v", got)
	}
}

func TestWrap_NoRows(t *testing.T) {
	got := Wrap(context.Background(), "M.G", sql.ErrNoRows, nil, DialectPostgres)
	st, ok := status.FromError(got)
	if !ok || st.Code() != codes.NotFound {
		t.Fatalf("got %v code=%v ok=%v", got, st.Code(), ok)
	}
}

func TestWrap_InternalOnUnknownError(t *testing.T) {
	var got error
	withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", errors.New("connection reset"), nil, DialectPostgres)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.Internal {
		t.Errorf("Code = %v, want Internal", st.Code())
	}
	// Original error string MUST NOT escape — protects
	// against schema/table leak per principle 2.
	if got.Error() == "connection reset" || contains(got.Error(), "connection reset") {
		t.Errorf("internal err leaked to client: %q", got.Error())
	}
}

func TestWrap_RegistryHit_FK_EmitsFEFriendlyCode(t *testing.T) {
	// FK violation: registry hit emits structured detail
	// with the FE-friendly code the codegen put there
	// (INVALID_VALUE for FK; the DB-layer FK_VIOLATION never
	// makes the wire). Raw DB error ALSO goes to errorx for
	// ops debugging.
	registry := &ConstraintRegistry{
		ByName: map[string]ConstraintInfo{
			"users_parent_id_fkey": {
				Field:   "parent_id",
				Code:    "INVALID_VALUE", // FE vocabulary, not "FK_VIOLATION"
				Message: "referenced record does not exist",
			},
		},
	}
	pgErr := &pgconn.PgError{
		Code:           "23503",
		ConstraintName: "users_parent_id_fkey",
		TableName:      "users",
	}
	var got error
	logged := withCapturedLog(t, func() {
		got = Wrap(context.Background(), "UserMutation.CreateUser", pgErr, registry, DialectPostgres)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("Code = %v, want InvalidArgument", st.Code())
	}
	details := st.Details()
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
	d, ok := details[0].(*w17pb.ErrorDetail)
	if !ok {
		t.Fatalf("detail type = %T", details[0])
	}
	if d.GetCode() != "INVALID_VALUE" {
		t.Errorf("code on wire must be FE-friendly: got %q, want INVALID_VALUE", d.GetCode())
	}
	if d.GetField() != "parent_id" {
		t.Errorf("Field = %q", d.GetField())
	}
	// T-grpcerr-1: a successfully-mapped constraint violation is a
	// routine, user-correctable failure — it must NOT be reported as
	// an exception (the pre-fix code unconditionally CaptureException'd
	// here, turning every dup/FK/CHECK into Sentry noise). The raw
	// constraint identity now rides the non-exception debug channel
	// (observx.ReportEvent, off by default), so the exception-fallback
	// log stays empty here.
	//
	// NOTE: this updates the prior golden, which asserted the raw
	// identity appeared in the exception log on the success path —
	// that was pinning the bug.
	if logged != "" {
		t.Errorf("mapped constraint violation must not raise an exception-level report; logged: %q", logged)
	}
}

func TestWrap_PgConstraint_RegistryHit_ByName(t *testing.T) {
	registry := &ConstraintRegistry{
		ByName: map[string]ConstraintInfo{
			"users_email_unique": {
				Field:   "email",
				Code:    "UNIQUE_VIOLATION", // UNIQUE keeps specific FE code
				Message: "email already exists",
			},
		},
	}
	pgErr := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "users_email_unique",
		TableName:      "users",
	}
	var got error
	withCapturedLog(t, func() {
		got = Wrap(context.Background(), "UserMutation.CreateUser", pgErr, registry, DialectPostgres)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("Code = %v, want InvalidArgument", st.Code())
	}
	details := st.Details()
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
	d, ok := details[0].(*w17pb.ErrorDetail)
	if !ok {
		t.Fatalf("detail type = %T, want *w17pb.ErrorDetail", details[0])
	}
	if d.GetField() != "email" || d.GetCode() != "UNIQUE_VIOLATION" || d.GetMessage() != "email already exists" {
		t.Errorf("detail mismatch: %+v", d)
	}
}

func TestWrap_PgConstraint_RegistryMiss_FailedPrecondition(t *testing.T) {
	// Constraint kind recognised, identity not in registry —
	// surfaces FailedPrecondition with bare context for
	// operator triage.
	pgErr := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "ghost_constraint",
		TableName:      "users",
	}
	got := Wrap(context.Background(), "M.G", pgErr, &ConstraintRegistry{}, DialectPostgres)
	st, _ := status.FromError(got)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("Code = %v, want FailedPrecondition", st.Code())
	}
	if !contains(st.Message(), "ghost_constraint") {
		t.Errorf("message should mention constraint name: %q", st.Message())
	}
}

// T-grpcerr-1: the registry-miss path is a genuine codegen gap (a
// DB constraint exists that wasn't registered from the schema IR).
// Unlike a mapped violation it MUST be reported as a real
// exception so operators see the gap — assert the report fires
// (in tests, observx with no reporter falls back to log.Printf's
// "error:" line) and carries the constraint identity.
func TestWrap_RegistryMiss_ReportsCodegenGap(t *testing.T) {
	pgErr := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "ghost_constraint",
		TableName:      "users",
	}
	var got error
	logged := withCapturedLog(t, func() {
		got = Wrap(context.Background(), "UserMutation.CreateUser", pgErr, &ConstraintRegistry{}, DialectPostgres)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("Code = %v, want FailedPrecondition", st.Code())
	}
	if !contains(logged, "ghost_constraint") {
		t.Errorf("codegen gap should be reported with the constraint identity; logged: %q", logged)
	}
	if !contains(logged, "registry gap") {
		t.Errorf("report should flag the codegen registry gap; logged: %q", logged)
	}
}

func TestWrap_SQLite_UniqueByColumns(t *testing.T) {
	// SQLite UNIQUE doesn't expose constraint name — registry
	// must hit via ByColumns key "<table>:<kind>:<cols>".
	registry := &ConstraintRegistry{
		ByColumns: map[string]ConstraintInfo{
			"users:unique:email": {
				Field: "email", Code: "UNIQUE_VIOLATION",
				Message: "email taken",
			},
		},
	}
	ce := &dialect.ConstraintError{
		Kind:    dialect.KindUnique,
		Table:   "users",
		Columns: []string{"email"},
	}
	info, ok := lookupRegistry(registry, ce)
	if !ok || info.Field != "email" {
		t.Fatalf("ByColumns lookup failed: %+v ok=%v", info, ok)
	}
}

func TestWrap_SQLite_FK_SoleAttribution(t *testing.T) {
	// SQLite FK exposes nothing — table is single-FK, sole
	// attribution wins. (Adapter returns Table="" for
	// SQLite, so this exercises the cross-table single-FK
	// fallback in lookupRegistry.)
	registry := &ConstraintRegistry{
		SoleFKByTable: map[string]ConstraintInfo{
			"orders": {
				Field: "user_id", Code: "FK_VIOLATION",
				Message: "user does not exist",
			},
		},
	}
	ce := &dialect.ConstraintError{Kind: dialect.KindFK}
	info, ok := lookupRegistry(registry, ce)
	if !ok || info.Field != "user_id" {
		t.Fatalf("SoleFK fallback failed: %+v ok=%v", info, ok)
	}
}

func TestWrap_SQLite_FK_MultiTableNoAttribution(t *testing.T) {
	// More than one FK in registry + adapter has no Table —
	// must NOT attribute to avoid wrong message.
	registry := &ConstraintRegistry{
		SoleFKByTable: map[string]ConstraintInfo{
			"orders":   {Field: "user_id"},
			"comments": {Field: "post_id"},
		},
	}
	ce := &dialect.ConstraintError{Kind: dialect.KindFK}
	if _, ok := lookupRegistry(registry, ce); ok {
		t.Error("multi-FK with no Table should NOT attribute")
	}
}

func TestWrap_SQLite_FK_SingleFKTableWithMultiFKElsewhere_NoAttribution(t *testing.T) {
	// Q55-grpcerr-1 — one single-FK table sits in SoleFKByTable while a
	// multi-FK table (absent from the map → MultiFKPresent=true) also
	// exists. A bare SQLite FK error (no table) must NOT be attributed to
	// the lone entry: it could just as well be the multi-FK table's
	// failure. len(SoleFKByTable)==1 alone is not enough.
	registry := &ConstraintRegistry{
		SoleFKByTable:  map[string]ConstraintInfo{"orders": {Field: "user_id"}},
		MultiFKPresent: true,
	}
	ce := &dialect.ConstraintError{Kind: dialect.KindFK}
	if info, ok := lookupRegistry(registry, ce); ok {
		t.Errorf("single-FK table + multi-FK table elsewhere must NOT attribute a table-less FK error; got %+v", info)
	}
}

func TestWrap_InternalErrorRoutesViaErrorx(t *testing.T) {
	// REV-031 Phase C-5 — unrecognised driver errors land as
	// Internal + go through gox/errorx for Sentry-aware
	// routing. In tests (no reporter configured), errorx
	// falls back to log.Printf — capture it.
	logged := withCapturedLog(t, func() {
		_ = Wrap(context.Background(), "M.G", errors.New("connection reset"), nil, DialectPostgres)
	})
	if !contains(logged, "M.G") || !contains(logged, "connection reset") {
		t.Errorf("Wrap should route raw err to errorx with method context; logged = %q", logged)
	}
}

func TestWrap_PgRetryable_Serialization(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "40001"}
	got := Wrap(context.Background(), "M.G", pgErr, nil, DialectPostgres)
	st, _ := status.FromError(got)
	if st.Code() != codes.Aborted {
		t.Errorf("Code = %v, want Aborted", st.Code())
	}
}

func TestWrap_PgRetryable_Deadlock(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "40P01"}
	got := Wrap(context.Background(), "M.G", pgErr, nil, DialectPostgres)
	st, _ := status.FromError(got)
	if st.Code() != codes.Aborted {
		t.Errorf("Code = %v, want Aborted", st.Code())
	}
}

func TestWrap_NilRegistry(t *testing.T) {
	pgErr := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "anything",
	}
	got := Wrap(context.Background(), "M.G", pgErr, nil, DialectPostgres)
	st, _ := status.FromError(got)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("Code = %v, want FailedPrecondition", st.Code())
	}
}

// Helper — case-sensitive substring without importing strings
// to keep the test file's import block tight; the prod Wrap
// already imports strings.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Avoid unused import.
var _ = protoadapt.MessageV1Of
