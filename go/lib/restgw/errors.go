package restgw

import (
	"context"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/i18n"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// HTTPStatusFromGRPCCode maps canonical gRPC status codes to
// HTTP status codes per the gRPC-to-HTTP convention every
// REST gateway in this project follows. Values match
// google.api / grpc-gateway conventions so client-side
// error-handling stays predictable.
//
// Unknown / unhandled codes fall through to 500.
func HTTPStatusFromGRPCCode(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499 // client closed request — non-standard but widely used.
	case codes.Unknown:
		return http.StatusInternalServerError
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.Aborted:
		return http.StatusConflict
	case codes.OutOfRange:
		return http.StatusBadRequest
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Internal:
		return http.StatusInternalServerError
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DataLoss:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// GRPCCodeName maps a gRPC status code to its canonical
// UPPER_SNAKE name — the `code` spelling every error envelope
// in this package emits (json.go documents the field as the
// canonical gRPC code name, e.g. "INVALID_ARGUMENT",
// "NOT_FOUND", so clients can branch on it without parsing
// prose). grpc's own codes.Code.String() returns PascalCase
// ("InvalidArgument", "NotFound"), so backend-propagated
// errors must be normalized here to stay byte-identical to the
// gateway-side hand-written paths (auth/scope/upload) and the
// generated handler prelude. Names follow the google.rpc.Code
// enum. Unknown codes fall through to "INTERNAL", matching the
// 500 fallback for non-status errors.
func GRPCCodeName(c codes.Code) string {
	switch c {
	case codes.OK:
		return "OK"
	case codes.Canceled:
		return "CANCELLED"
	case codes.Unknown:
		return "UNKNOWN"
	case codes.InvalidArgument:
		return "INVALID_ARGUMENT"
	case codes.DeadlineExceeded:
		return "DEADLINE_EXCEEDED"
	case codes.NotFound:
		return "NOT_FOUND"
	case codes.AlreadyExists:
		return "ALREADY_EXISTS"
	case codes.PermissionDenied:
		return "PERMISSION_DENIED"
	case codes.ResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case codes.FailedPrecondition:
		return "FAILED_PRECONDITION"
	case codes.Aborted:
		return "ABORTED"
	case codes.OutOfRange:
		return "OUT_OF_RANGE"
	case codes.Unimplemented:
		return "UNIMPLEMENTED"
	case codes.Internal:
		return "INTERNAL"
	case codes.Unavailable:
		return "UNAVAILABLE"
	case codes.DataLoss:
		return "DATA_LOSS"
	case codes.Unauthenticated:
		return "UNAUTHENTICATED"
	}
	return "INTERNAL"
}

// WriteGRPCError translates a gRPC error returned by the backend dial into an
// HTTP response.
//
// # Two audiences, two strings
//
// `status.Message()` is a DEVELOPER-facing field — gRPC's own convention, and
// what `grpcerr.Wrap` writes for: it carries the `<Service>.<Method>` prefix an
// operator needs to place a failure. It is therefore NEVER the string this
// sends to a client. It goes to observability, every time.
//
// What the client gets is the `w17.ErrorDetail` the backend attached: a stable
// `code` to dispatch on and a message already translated through the project's
// catalog. Handlers that attach nothing get a generic sentence for their gRPC
// code rather than the developer's.
//
// That inverts the old failure mode. This used to emit the message verbatim
// and genericise only what pattern-matched as transport, so an unrecognised
// technical string was SHOWN — fail-open, and every new message shape was a
// new way to leak one. Now nothing reaches a reader unless somebody wrote it
// for a reader.
func WriteGRPCError(w http.ResponseWriter, err error) {
	// No context, so no language: the fallback sentence stays in the source
	// locale. A detail is unaffected — grpcerr translated it with the
	// REQUEST's context, which is where the language actually is. Prefer
	// WriteGRPCErrorCtx wherever a request is in scope.
	WriteGRPCErrorCtx(context.Background(), w, err)
}

// WriteGRPCErrorCtx is WriteGRPCError with the request's context, so the
// fallback sentence is translated too.
func WriteGRPCErrorCtx(ctx context.Context, w http.ResponseWriter, err error) {
	if err == nil {
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "nil error")
		return
	}
	st, ok := status.FromError(err)
	if !ok {
		// A non-status error (transport / dial / unexpected) — its text
		// can carry internal topology (backend addrs, driver detail). Log
		// it server-side and return a generic message rather than
		// reflecting it to the client (restgw-sec-3).
		observx.ReportError(context.Background(), err)
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}

	// The operator's copy, always and regardless of outcome. It is the only
	// place the method prefix and any backend prose is allowed to land.
	observx.ReportError(context.Background(), err)

	httpStatus := HTTPStatusFromGRPCCode(st.Code())
	// B25-restgw-1: carry the backend's *w17.ErrorDetail field violations into
	// the envelope. grpcerr.Wrap attaches them for DB constraint violations a
	// gateway pre-flight can't catch (e.g. a UNIQUE email); dropping them leaves
	// the client unable to map the failure to a form field.
	details := fieldErrorsFromStatus(st)

	code, message := clientFacing(ctx, st, details)
	if len(details) > 0 {
		WriteErrorWithDetails(w, httpStatus, code, message, details)
		return
	}
	WriteError(w, httpStatus, code, message)
}

// reportByCode sends a failure down the channel that matches what it IS.
//
// Server faults are exceptions: somebody has to look. Everything else is an
// expected outcome of a request — the caller asked for something missing, or
// sent something invalid — and belongs on the non-exception channel, where it
// is still retrievable when triaging without marking a span failed or paging
// anyone.
func reportByCode(ctx context.Context, c codes.Code, err error) {
	switch c {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable:
		observx.ReportError(ctx, err)
	default:
		observx.ReportEvent(ctx, err)
	}
}

// clientFacing picks the code and sentence a person should see.
//
// Preference order, and each step is a deliberate demotion:
//
//  1. A REQUEST-LEVEL detail — one with a code and no field. That is the
//     backend saying "here is this failure, phrased for your user", and it is
//     already translated.
//  2. The gRPC code's own generic sentence. A handler that attached nothing
//     has said nothing fit to show, so the envelope says only as much as the
//     code does.
//
// `status.Message()` is never a candidate. It belongs to the operator.
func clientFacing(ctx context.Context, st *status.Status, details []FieldError) (string, string) {
	for _, d := range details {
		if d.Field == "" && d.Code != "" && d.Message != "" {
			return d.Code, d.Message
		}
	}
	// ONE table, in grpcerr, shared with the side that builds details. Two
	// copies of one sentence drift, and here the drift would be silent in a
	// particular way: only one copy is harvested into the catalogs, so the
	// other renders English in every declared language with nothing to say so.
	return GRPCCodeName(st.Code()), i18n.T(ctx, grpcerr.UserMsgid(st.Code()), nil)
}

// The transport scrub that used to live here is GONE, not relocated.
//
// It genericised `Unavailable` and anything whose text matched a transport
// marker (`dial tcp`, `connection refused`, …) and passed everything else
// through. That is fail-OPEN: an unrecognised technical string was SHOWN, and
// every new message shape was a new way to leak one. It also could not answer
// the question that actually mattered — a service name reveals no topology and
// sailed through every check it made.
//
// Nothing now reaches a reader unless somebody wrote it for a reader, so there
// is nothing left to pattern-match and no list to keep up to date.

// fieldErrorsFromStatus extracts the *w17.ErrorDetail entries a status carries
// (attached by grpcerr.Wrap via status.WithDetails) as REST FieldErrors. Empty
// when the status carries no such detail. Shared by the unary + WS error paths.
func fieldErrorsFromStatus(st *status.Status) []FieldError {
	var out []FieldError
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			out = append(out, FieldError{Field: ed.GetField(), Code: ed.GetCode(), Message: ed.GetMessage()})
		}
	}
	return out
}
