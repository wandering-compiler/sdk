package grpcerr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
	"github.com/wandering-compiler/sdk/go/core/grpcerr/dialect"
	"github.com/wandering-compiler/sdk/go/core/observx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
)

// ConstraintInfo is one registry entry — the payload Wrap
// emits as a *w17.ErrorDetail when a known DB constraint
// fires. Generated at codegen time per binary from the
// schema IR plus `(w17.field|db.table).validation_messages`
// annotations.
//
// Field is the proto field path the violation pertains to
// ("email", "user.id"). Empty for table-level constraints
// where no single field maps cleanly.
//
// Code is the FE-vocabulary code — describes the failure
// from the frontend's perspective, NOT a DB-layer concept.
// Defaults: `INVALID_VALUE` for UNIQUE / FK / CHECK /
// EXCLUSION (the FE just knows "this value isn't valid for
// this field, see message"); `REQUIRED_VIOLATION` for
// NOT_NULL (mirrors Stage-1 REQUIRED so FE has one consistent
// "field missing" handler). Authors can pick their own codes
// via custom annotations (future); today the catalog drives.
//
// Message is the resolved user-facing string (defaults
// catalog or author override).
//
// The raw DB error always goes to gox/errorx alongside the
// returned status — devs see the original constraint
// identity (PG ConstraintName, raw err) in Sentry/stderr for
// debugging, while the client sees only the FE-vocabulary
// code + author-grade message.
type ConstraintInfo struct {
	Field   string
	Code    string
	Message string
}

// ConstraintRegistry maps constraint identity to the
// ConstraintInfo to emit. Built at codegen time from the
// service's schema IR; passed by reference into every
// generated handler's Wrap call so the lookup is one map
// hit on the failure path (zero cost on happy path).
//
// Dual-indexed per the portability decision (see
// `docs/decisions/db-error-classification-portability.md`):
//
//   - ByName covers PG (structured field), MySQL, MSSQL,
//     Oracle, SQLite-CHECK — every dialect that exposes
//     constraint name in some form.
//   - ByColumns is the fallback for SQLite UNIQUE +
//     SQLite/MySQL NOT_NULL where only columns are exposed.
//   - SoleFKByTable is the SQLite FK fallback — when the
//     dialect tells us only "an FK failed" without naming
//     it, we attribute to the table's sole FK if there is
//     exactly one. Multi-FK SQLite tables surface the
//     generic Internal fallback (and codegen warns at
//     compile time).
type ConstraintRegistry struct {
	// ByName key = constraint name (e.g. "users_email_unique").
	ByName map[string]ConstraintInfo

	// ByColumns key = "<table>:<kind>:<col1>,<col2>" (joined,
	// no spaces). Kind is one of dialect.Kind* constants.
	// Generated alongside ByName so SQLite paths land here.
	ByColumns map[string]ConstraintInfo

	// SoleFKByTable key = table name. Populated only for
	// tables with exactly one FK on SQLite-targeted
	// services. Multi-FK tables are absent (codegen warns).
	SoleFKByTable map[string]ConstraintInfo

	// MultiFKPresent reports whether any table in this service has
	// ≥2 foreign keys. SQLite's bare "FOREIGN KEY constraint failed"
	// error carries no table, so Wrap can only attribute it when the
	// WHOLE service has exactly one FK. `len(SoleFKByTable) == 1`
	// alone doesn't prove that — a single-FK table can coexist with a
	// multi-FK table (which is absent from SoleFKByTable), and the
	// lone entry would then capture the multi-FK table's failures too
	// (Q55-grpcerr-1). The codegen sets this so the bare-FK fallback
	// stays off whenever attribution is ambiguous.
	MultiFKPresent bool

	// SoleNotNullByColumn key = column name. The MySQL NOT_NULL
	// error ("Column 'x' cannot be null") names only the column,
	// NOT the table, so the table-keyed ByColumns lookup can't
	// resolve it (Q47-grpc-1). This degraded fallback attributes
	// a table-less NOT_NULL to a column when exactly ONE table in
	// the bundle has a NOT_NULL on that single column (so there's
	// no ambiguity); columns that several tables share stay absent
	// → Wrap falls through to FailedPrecondition. Mirrors the
	// SoleFKByTable single-match philosophy.
	SoleNotNullByColumn map[string]ConstraintInfo
}

// Dialect identifies which adapter to use when parsing the
// driver error. The codegen layer knows which dialect each
// service runs against and embeds it as a constant in the
// emitted Wrap call site.
type Dialect int

const (
	DialectUnknown Dialect = iota
	DialectPostgres
	DialectMySQL
	DialectSQLite
)

// Wrap is the canonical entry point for translating a DB
// error into a gRPC status. Replaces the old PgError path
// (REV-026 Phase A) with portable, structured-detail-aware
// emit (REV-031 Phase C-3).
//
// Precedence (first match wins):
//
//  1. err == nil → return nil (terse call sites).
//  2. sql.ErrNoRows → status.Error(NotFound) — same as
//     PgError, no constraint context to emit.
//  3. Per-dialect adapter → ConstraintError → registry
//     lookup → status.WithDetails(*w17.ErrorDetail). This
//     is the structured-emit path; client sees the same
//     {field, code, message} envelope as Stage-1.
//  4. Constraint matched but unknown to registry →
//     status.Error(FailedPrecondition) carrying the bare
//     constraint name, AND an observx.ReportError (this is a
//     genuine codegen gap — a constraint exists in DB but
//     wasn't registered — so operators should see it as an
//     exception). Client sees a generic prose.
//  5. Driver error not a constraint violation → log
//     internal + return status.Error(Internal,
//     "<method>: internal error"). Original err string
//     does NOT escape to client — protects against schema
//     name / table / column leakage. (Internal logging
//     scaffolding lands with C-3+ — today returns the
//     Internal status without forwarding to Sentry; trace_id
//     correlation is C-6.)
//
// Method identifier ("UserMutation.CreateUser") prefixes
// the message in cases 4 and 5 so operator logs carry
// failing-method context. Cases 1-3 keep the message
// terse — the structured detail carries the field-level
// information.
func Wrap(ctx context.Context, method string, err error, registry *ConstraintRegistry, d Dialect) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return status.Error(codes.NotFound, method+": not found")
	}
	// Transient/retryable classes BEFORE constraint parsing —
	// serialization failures and deadlocks aren't constraint
	// violations but the gox retry layer needs codes.Aborted
	// to recognise them as retryable. Preserves the REV-026
	// Phase A PgError classification so retry semantics don't
	// regress when the codegen swaps PgError → Wrap.
	if isRetryable(err, d) {
		return status.Errorf(codes.Aborted, "%s: retryable failure", method)
	}
	// Context cancellation / deadline BEFORE constraint parsing —
	// a DB call aborted by a cancelled or timed-out context is not
	// a server fault, so it must NOT map to codes.Internal nor get
	// reported to observx (that would be pure Sentry noise on every
	// client disconnect / slow query). Surface the canonical gRPC
	// codes the caller already expects for these conditions.
	if errors.Is(err, context.Canceled) {
		return status.Errorf(codes.Canceled, "%s: canceled", method)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Errorf(codes.DeadlineExceeded, "%s: deadline exceeded", method)
	}
	ce, ok := parseByDialect(err, d)
	if !ok {
		// Not a constraint error — internal failure.
		// User-facing message stays generic; raw err goes
		// to lib/observx (Sentry + OTel active span both get
		// tagged with service metadata + trace_id; stderr
		// fallback when neither exporter is configured) —
		// REV-031 Phase C-6.
		observx.ReportError(ctx, fmt.Errorf("%s: %w", method, err))
		return status.Errorf(codes.Internal, "%s: internal error", method)
	}
	info, found := lookupRegistry(registry, ce)
	if !found {
		// Constraint kind known, identity not in registry — a
		// genuine codegen gap (a constraint exists in the DB that
		// wasn't registered from the schema IR). Report it as a
		// real observx exception so operators SEE the gap, then
		// surface FailedPrecondition with bare details so it can
		// be grepped from logs too.
		observx.ReportError(ctx, fmt.Errorf(
			"%s: unmapped %s constraint (name=%q table=%q cols=%v) — codegen registry gap",
			method, ce.Kind, ce.Name, ce.Table, ce.Columns))
		detail := ce.Kind
		if ce.Name != "" {
			detail = ce.Kind + " constraint " + ce.Name
		} else if ce.Table != "" && len(ce.Columns) > 0 {
			detail = ce.Kind + " on " + ce.Table + "(" + strings.Join(ce.Columns, ",") + ")"
		}
		return status.Errorf(codes.FailedPrecondition,
			"%s: unmapped %s", method, detail)
	}
	// Successfully-mapped constraint violation — a routine,
	// user-correctable failure (dup email, CHECK, NOT NULL), NOT a
	// server fault. It must NOT raise a Sentry exception nor mark
	// the OTel span failed (mirrors how Stage-1 proto-validation
	// failures, which also return InvalidArgument, stay off the
	// exception channel). The original constraint identity (PG
	// ConstraintName, table, raw SQLSTATE message) still rides the
	// non-exception debug channel so devs can pull it up when
	// triaging — gated behind W17_OBSERVX_DEBUG, off by default.
	observx.ReportEvent(ctx, fmt.Errorf("%s: %s constraint hit (name=%q field=%q): %w",
		method, ce.Kind, ce.Name, info.Field, err))
	st := status.New(codeForKind(ce.Kind), method+": "+ce.Kind+" violation")
	with, errWith := st.WithDetails(protoadapt.MessageV1Of(&w17pb.ErrorDetail{
		Field:   info.Field,
		Code:    info.Code,
		Message: info.Message,
	}))
	if errWith != nil {
		// coverage-exempt: unreachable defensive guard — WithDetails
		// only errors if the detail fails to marshal, but ErrorDetail
		// is a well-typed static proto that always marshals.
		// Detail attach failed — defensive; ErrorDetail is
		// well-typed proto so this is should-not-happen.
		// Fall back to bare status with method context.
		return st.Err()
	}
	return with.Err()
}

// parseByDialect dispatches to the right adapter. Falls
// through to ok=false for DialectUnknown so callers without
// a known dialect get the internal-error path.
func parseByDialect(err error, d Dialect) (*dialect.ConstraintError, bool) {
	switch d {
	case DialectPostgres:
		return dialect.ParsePg(err)
	case DialectMySQL:
		return dialect.ParseMySQL(err)
	case DialectSQLite:
		return dialect.ParseSQLite(err)
	}
	return nil, false
}

// lookupRegistry runs the precedence cascade described in
// the type doc on ConstraintRegistry: ByName → ByColumns →
// SoleFKByTable for the SQLite degraded case.
func lookupRegistry(r *ConstraintRegistry, ce *dialect.ConstraintError) (ConstraintInfo, bool) {
	if r == nil {
		return ConstraintInfo{}, false
	}
	if ce.Name != "" && r.ByName != nil {
		if info, ok := r.ByName[ce.Name]; ok {
			return info, true
		}
	}
	if ce.Table != "" && len(ce.Columns) > 0 && r.ByColumns != nil {
		key := ce.Table + ":" + ce.Kind + ":" + strings.Join(ce.Columns, ",")
		if info, ok := r.ByColumns[key]; ok {
			return info, true
		}
	}
	// Q47-grpc-1: MySQL NOT_NULL names only the column (no table), so the
	// table-keyed ByColumns lookup above never matches. Attribute it to a
	// column when exactly one table owns a NOT_NULL on that column (the
	// codegen only populates SoleNotNullByColumn for unambiguous columns).
	if ce.Kind == dialect.KindNotNull && ce.Table == "" && len(ce.Columns) == 1 && r.SoleNotNullByColumn != nil {
		if info, ok := r.SoleNotNullByColumn[ce.Columns[0]]; ok {
			return info, true
		}
	}
	if ce.Kind == dialect.KindFK && ce.Table != "" && r.SoleFKByTable != nil {
		// Wrap-attribution: SQLite's bare "FK failed" — the
		// adapter doesn't fill Table either, so this branch
		// fires for empty Table too via the next if-block.
		if info, ok := r.SoleFKByTable[ce.Table]; ok {
			return info, true
		}
	}
	if ce.Kind == dialect.KindFK && ce.Table == "" && r.SoleFKByTable != nil {
		// SQLite FK with no table info — only attribute when the
		// service has exactly one FK across ALL tables: one entry in
		// SoleFKByTable AND no multi-FK table elsewhere (Q55-grpcerr-1).
		// Otherwise the lone single-FK entry would mis-attribute a
		// multi-FK table's failure; fall through to FailedPrecondition.
		if len(r.SoleFKByTable) == 1 && !r.MultiFKPresent {
			for _, info := range r.SoleFKByTable {
				return info, true
			}
		}
	}
	return ConstraintInfo{}, false
}

// isRetryable detects transient DB errors the gox retry
// layer treats as Aborted-and-retryable. Per dialect:
//
//	PG  : 40001 serialization_failure, 40P01 deadlock_detected
//	MySQL: 1213 deadlock, 1205 lock wait timeout
//	SQLite: 5 SQLITE_BUSY, 6 SQLITE_LOCKED (basic codes —
//	        the modernc driver returns these via Code(); not
//	        wired today since SQLite is single-writer in
//	        practice and these are rare in real code)
//
// Other dialects (MSSQL, Oracle) get their patterns when
// adapters land. The check is positive-list — unknown codes
// fall through to the constraint-parser.
func isRetryable(err error, d Dialect) bool {
	switch d {
	case DialectPostgres:
		var pgxErr *pgconn.PgError
		if errors.As(err, &pgxErr) {
			return pgxErr.Code == "40001" || pgxErr.Code == "40P01"
		}
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			return pqErr.Code == "40001" || pqErr.Code == "40P01"
		}
	case DialectMySQL:
		var me *mysql.MySQLError
		if errors.As(err, &me) {
			return me.Number == 1213 || me.Number == 1205
		}
	}
	return false
}

// codeForKind maps the dialect-normalised Kind to the gRPC
// status code clients see. Validation-class violations
// (UNIQUE, FK, NOT_NULL, CHECK, EXCLUSION) all surface as
// InvalidArgument so the client treats them as "fix and
// retry" rather than infrastructure failures — matches the
// Stage-1 path's InvalidArgument shape so both stages
// produce one consistent client experience.
//
// Earlier (REV-026 Phase A) PgError split: UNIQUE/EXCLUSION
// → AlreadyExists, FK → FailedPrecondition. C-3 collapses
// these into InvalidArgument because the structured detail
// (`code: UNIQUE_VIOLATION` etc.) carries the discrimination;
// the gRPC top-level code can stay uniform across the
// validation-class violations.
func codeForKind(kind string) codes.Code {
	switch kind {
	case dialect.KindUnique, dialect.KindFK, dialect.KindCheck,
		dialect.KindNotNull, dialect.KindExclusion:
		return codes.InvalidArgument
	}
	return codes.Internal
}
