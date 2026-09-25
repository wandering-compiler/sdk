package grpcerr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
	"github.com/wandering-compiler/sdk/go/core/grpcerr/dialect"
	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/i18n"
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
// ─── User-facing codes for the branches that are not constraint hits ───
//
// Vocabulary in the same spirit as validation's `defaultCodes`: each names the
// problem FROM THE CALLER'S SIDE, not the mechanism that produced it. A client
// dispatches on these; the catalog translates the sentence beside them.
//
// They exist because six of this function's branches used to return a bare
// string and nothing else — so there was nothing for a client to branch on and
// nothing for the catalog to translate, only a developer's sentence that the
// gateway then showed to a person.
const (
	CodeNotFound        = "NOT_FOUND"
	CodeConflictRetry   = "CONFLICT_RETRY"
	CodeCanceled        = "REQUEST_CANCELED"
	CodeTimeout         = "TIMEOUT"
	CodeValueOutOfRange = "VALUE_OUT_OF_RANGE"
	CodeInternal        = "INTERNAL"
)

// UserMsgid is the sentence a PERSON gets for a gRPC code when nobody wrote
// them a better one.
//
// Deliberately vague: this is the fallback, and a vague sentence beside an
// exact `code` is more useful than a precise one about the wrong thing. The
// code is what a client dispatches on; this is what someone reads while an
// operator checks the logs.
//
// One table, used by both sides — this package when it builds a detail, and
// the gateway when a status carries none. It was briefly written twice, which
// is the shape that put the same certificate-pin gap in three places earlier
// this week: two copies of one rule drift, and the drift is silent.
func UserMsgid(c codes.Code) string {
	switch c {
	case codes.NotFound:
		return "The item you asked for does not exist."
	case codes.InvalidArgument, codes.OutOfRange, codes.FailedPrecondition:
		return "The request could not be accepted as sent."
	case codes.PermissionDenied:
		return "You do not have access to this."
	case codes.Unauthenticated:
		return "You are not signed in."
	case codes.AlreadyExists, codes.Aborted:
		return "Another change reached this first. Please try again."
	case codes.ResourceExhausted:
		return "Too many requests. Please wait and try again."
	case codes.DeadlineExceeded:
		return "The request took too long and was stopped."
	case codes.Canceled:
		return "The request was cancelled before it finished."
	case codes.Unavailable:
		return "The service is temporarily unavailable."
	case codes.Unimplemented:
		return "This is not available."
	}
	return "Something went wrong on our side."
}

// Vocabulary is every msgid this package can put in front of a person.
//
// Harvested into each project's `.po` alongside the validation defaults, so
// these sentences are translatable rather than permanently English. A msgid
// with no catalog entry falls back to itself, so a project that never
// translates reads exactly as it does today.
func Vocabulary() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range []codes.Code{
		codes.NotFound, codes.InvalidArgument, codes.PermissionDenied,
		codes.Unauthenticated, codes.Aborted, codes.ResourceExhausted,
		codes.DeadlineExceeded, codes.Canceled, codes.Unavailable,
		codes.Unimplemented, codes.Internal,
	} {
		if m := UserMsgid(c); !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// ForUser builds an error that says one thing to an operator and another to a
// person — the shape every hand-written handler should reach for.
//
//	return grpcerr.ForUser(ctx, codes.FailedPrecondition,
//	    "BillingService.Charge", "subscription is not active",
//	    "SUBSCRIPTION_INACTIVE", "This subscription is not active.")
//
// A handler that returns a bare `status.Error` still works; its prose simply
// stays with the operator, and the caller gets the generic sentence for the
// gRPC code. That is the intended demotion — a message nobody marked as
// user-facing is treated as though nobody wrote it for a user, because nobody
// did.
func ForUser(ctx context.Context, c codes.Code, method, dev, code, userMsgid string) error {
	return forCaller(ctx, c, method, dev, code, userMsgid)
}

// forCaller builds the two-audience error this package owes both of its
// readers.
//
// `dev` becomes the status message and keeps the method prefix — that is the
// string an operator reads in Sentry, and stripping it would leave `not found`
// with no origin. `userMsgid` becomes an ErrorDetail the GATEWAY renders for a
// person, translated through the same catalog the validation path uses.
//
// One string cannot serve both. That is the whole defect this replaces: the
// gateway was showing the operator's copy because it was the only copy.
func forCaller(ctx context.Context, c codes.Code, method, dev, code, userMsgid string) error {
	st := status.New(c, method+": "+dev)
	with, err := st.WithDetails(protoadapt.MessageV1Of(&w17pb.ErrorDetail{
		Code:    code,
		Message: i18n.T(ctx, userMsgid, nil),
	}))
	if err != nil {
		// coverage-exempt: WithDetails only fails if the detail cannot
		// marshal, and ErrorDetail is a static well-typed proto.
		return st.Err()
	}
	return with.Err()
}

func Wrap(ctx context.Context, method string, err error, registry *ConstraintRegistry, d Dialect) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return forCaller(ctx, codes.NotFound, method, "not found", CodeNotFound, UserMsgid(codes.NotFound))
	}
	// Transient classes BEFORE constraint parsing — a serialization
	// failure or deadlock (40001 / 40P01) is not a constraint
	// violation, and codes.Aborted is the gRPC code for it.
	// Preserves the REV-026 Phase A PgError classification so the
	// mapping didn't regress when codegen swapped PgError → Wrap.
	//
	// "Retryable" here names the CONDITION, not a mechanism: nothing
	// in this repo retries it, and that is deliberate, not a gap
	// (T3-7 pass #9 D-F7 — this comment used to promise "the gox
	// retry layer", which does not exist and never did). The channel
	// retry policy every w17 client installs
	// (`lib/grpcclient.DefaultServiceConfig`) retries UNAVAILABLE and
	// nothing else, pinned by
	// TestDefaultServiceConfig_RetriesOnlyUnavailable, because
	// UNAVAILABLE is the one code that proves the server never got
	// the request.
	//
	// Aborted must not join it, at this layer, for two reasons that
	// outlive any tuning: the method may have adopted a CALLER's
	// transaction, which the same serialization failure has already
	// poisoned (every further statement on it fails 25P02), so a
	// transparent replay of one RPC cannot recover it; and a
	// transparent replay would re-run whatever non-DB work the
	// calling business handler had already done. The unit that has
	// to be retried is the transaction, and only its owner knows
	// where that starts — so an Aborted is a signal TO THE CALLER to
	// restart its unit of work, and callers of a method that
	// declares `tx_isolation: SERIALIZABLE` should expect to.
	if isRetryable(err, d) {
		return forCaller(ctx, codes.Aborted, method, "retryable failure", CodeConflictRetry, UserMsgid(codes.Aborted))
	}
	// Context cancellation / deadline BEFORE constraint parsing —
	// a DB call aborted by a cancelled or timed-out context is not
	// a server fault, so it must NOT map to codes.Internal nor get
	// reported to observx (that would be pure Sentry noise on every
	// client disconnect / slow query). Surface the canonical gRPC
	// codes the caller already expects for these conditions.
	if errors.Is(err, context.Canceled) {
		return forCaller(ctx, codes.Canceled, method, "canceled", CodeCanceled, UserMsgid(codes.Canceled))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return forCaller(ctx, codes.DeadlineExceeded, method, "deadline exceeded", CodeTimeout, UserMsgid(codes.DeadlineExceeded))
	}
	// SQL "data exception" class — a value the CALLER supplied that the
	// column cannot hold. It is not a constraint violation (no constraint
	// name, so the registry has nothing to look up) and it is not a
	// server fault, but it fell into the `!ok` branch below and came out
	// as codes.Internal PLUS an observx exception. So an over-long string
	// answered a 500 and paged whoever watches Sentry.
	//
	// The message is generic and names no field on purpose: PG reports
	// neither the column nor the value for 22001 / 22003, so anything
	// more specific would be invented. A project that wants a field-named
	// refusal declares the rule on the request message, where the
	// generated validation answers first — which is why this is the
	// FALLBACK and not the intended path.
	if isDataException(err, d) {
		return forCaller(ctx, codes.InvalidArgument, method,
			"a value does not fit the column it was written to", CodeValueOutOfRange, UserMsgid(codes.InvalidArgument))
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
		return forCaller(ctx, codes.Internal, method, "internal error", CodeInternal, UserMsgid(codes.Internal))
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
		// A detail here too. This branch was missed when the other seven
		// got one, and it is the one that most needs it: its developer
		// message names the CONSTRAINT, the TABLE and the COLUMNS, so any
		// writer that still reflects status.Message() hands a caller the
		// database's own vocabulary. With a detail attached, every writer
		// has something to render instead.
		return forCaller(ctx, codes.FailedPrecondition, method,
			"unmapped "+detail, CodeInternal, UserMsgid(codes.FailedPrecondition))
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
	// The status MESSAGE names the field and says what is wrong with it.
	// It used to read `<method>: not_null violation` — a sentence that
	// names no column, no request field, and leaks a database word into an
	// API whose callers see fields, not columns. The mapping was not
	// missing: `info` carries exactly this, and it was already attached as
	// an ErrorDetail — but a caller reading the status message (which is
	// what a REST gateway surfaces, and what a human reads first) got none
	// of it. deinvo reported this on 2026-08-30 after a `customer_id` they
	// omitted came back as `DocumentMutation.CreateDocument: not_null
	// violation`.
	//
	// `info.Message` is written to follow the field name — "is required",
	// "already exists" — so field + message composes into the sentence the
	// detail channel was already carrying. The constraint KIND stays on
	// the debug event above, where an operator triaging by SQLSTATE looks;
	// it is not something the caller can act on.
	//
	// The message is a MSGID, resolved here rather than shipped verbatim.
	// Stage-1 validation (the emitted per-field checks) has resolved its
	// messages at runtime through `i18n.T(ctx, …)` since REV-149 P1.5, and
	// the constraint registry's strings come out of the SAME catalog
	// (`lib/validation`.defaults) — they are extracted into the domain's
	// `.po` files alongside them. Writing them as Go literals meant one
	// declaration answered a Czech caller in Czech from the request-shape
	// check and in English from the database constraint, for the same rule
	// (T2-6 pass #10, D10-5). `i18n.T` falls back to the bare msgid, so a
	// project with no catalog reads exactly as it did before.
	//
	// `docs/specs/runtime/error-envelope.md` still describes the wire as
	// "English defaults"; that text predates P1.5 by ten days and was
	// already half-false. It is corrected alongside this.
	message := info.Message
	if message != "" {
		message = i18n.T(ctx, message, nil)
	}
	detail := ce.Kind + " violation"
	if info.Field != "" && message != "" {
		detail = info.Field + " " + message
	}
	st := status.New(codeForKind(ce.Kind), method+": "+detail)
	with, errWith := st.WithDetails(protoadapt.MessageV1Of(&w17pb.ErrorDetail{
		Field:   info.Field,
		Code:    info.Code,
		Message: message,
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
// isDataException reports whether err is a SQL class-22 "data
// exception": the caller's VALUE is wrong for the column, as opposed to
// a constraint being violated or the server failing.
//
// Deliberately narrow. Only the codes whose cause is unambiguously a
// supplied value are listed — a string too long, a number out of range,
// a text literal that will not parse as its target type, a malformed
// datetime. The rest of class 22 (division by zero, substring error,
// …) can equally be the server's own SQL and stays on the internal
// path, where it is reported.
func isDataException(err error, d Dialect) bool {
	switch d {
	case DialectPostgres:
		var pgxErr *pgconn.PgError
		if errors.As(err, &pgxErr) {
			return isPgDataException(pgxErr.Code)
		}
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			return isPgDataException(string(pqErr.Code))
		}
	case DialectMySQL:
		var me *mysql.MySQLError
		if errors.As(err, &me) {
			//  1406 ER_DATA_TOO_LONG
			//  1264 ER_WARN_DATA_OUT_OF_RANGE
			//  1366 ER_TRUNCATED_WRONG_VALUE_FOR_FIELD
			//  1292 ER_TRUNCATED_WRONG_VALUE (bad datetime / number literal)
			return me.Number == 1406 || me.Number == 1264 || me.Number == 1366 || me.Number == 1292
		}
	}
	return false
}

// isPgDataException lists the class-22 SQLSTATEs attributable to the
// caller's value:
//
//	22001  string_data_right_truncation   — value longer than the column
//	22003  numeric_value_out_of_range     — number outside its precision
//	22007  invalid_datetime_format        — unparseable date/time literal
//	22P02  invalid_text_representation    — unparseable value for the type
func isPgDataException(code string) bool {
	switch code {
	case "22001", "22003", "22007", "22P02":
		return true
	}
	return false
}

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
