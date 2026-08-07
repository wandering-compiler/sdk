package restgw

import (
	"context"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/observx"
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

// WriteGRPCError translates a gRPC error returned by the
// backend dial into an HTTP response. Non-status errors
// (network blip, marshaling, …) fall through to 500
// `INTERNAL`. Status errors map their code to the canonical
// HTTP status + emit the error message verbatim.
func WriteGRPCError(w http.ResponseWriter, err error) {
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
	// B25-restgw-1: carry the backend's *w17.ErrorDetail field violations into
	// the envelope. grpcerr.Wrap attaches them for DB constraint violations a
	// gateway pre-flight can't catch (e.g. a UNIQUE email); dropping them leaves
	// the client unable to map the failure to a form field.
	if details := fieldErrorsFromStatus(st); len(details) > 0 {
		WriteErrorWithDetails(w, HTTPStatusFromGRPCCode(st.Code()), GRPCCodeName(st.Code()), st.Message(), details)
		return
	}
	WriteError(w, HTTPStatusFromGRPCCode(st.Code()), GRPCCodeName(st.Code()), scrubTransportMessage(st, err))
}

// transportMessageMarkers are fragments grpc-go puts in the messages it
// synthesises for connection failures. They are not phrases our own
// handlers produce (grpcerr.Wrap authors every message it emits), so a
// match means the text came from the transport and carries the dial
// target.
var transportMessageMarkers = []string{
	"connection error:",
	"transport:",
	"dial tcp",
	"dial unix",
	"latest balancer error",
	"lookup ",
}

// scrubTransportMessage returns the message safe to reflect to the client.
//
// restgw-sec-3 guards the non-status branch above on the grounds that a
// transport error's text carries internal topology. The guard missed the
// case it was written for: grpc-go reports a failed dial as a STATUS
// error — picker_wrapper wraps the balancer's last error in
// status.Error(codes.Unavailable, err.Error()) — so the real transport
// failures arrived as statuses and went out verbatim, backend host and
// port included. A client would read
// `dial tcp app-storage:9090: connect: connection refused` off a 503.
//
// The scrub is deliberately narrow, because most status messages ARE
// worth showing: grpcerr.Wrap authors them ("…: not found", constraint
// prose), and B25-restgw-1 carries field violations a form binds to.
// Genericising everything would gut that. Two rules instead:
//
//   - Unavailable is always genericised. Our handlers never raise it with
//     prose a caller acts on — the code is the actionable part — and a
//     transport-raised one is indistinguishable from a handler-raised one.
//   - Any code whose message bears a transport marker is genericised, so
//     the balancer error that rides out on DeadlineExceeded during an
//     outage is covered too.
//
// The full text always reaches observability; only the client's copy is
// reduced.
func scrubTransportMessage(st *status.Status, err error) string {
	msg := st.Message()
	if st.Code() != codes.Unavailable && !hasTransportMarker(msg) {
		return msg
	}
	observx.ReportError(context.Background(), err)
	if st.Code() == codes.Unavailable {
		return "upstream unavailable"
	}
	return "upstream request failed"
}

func hasTransportMarker(msg string) bool {
	for _, m := range transportMessageMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

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
