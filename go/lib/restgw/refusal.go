package restgw

import (
	"context"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// The gateway's OWN refusals, with the two audiences separated the way
// WriteGRPCErrorCtx separates them for a backend's.
//
// WriteGRPCErrorCtx only governs what a BACKEND returned. Every refusal the
// gateway decides by itself — a missing permission, a path param that is not a
// number, an auth response it could not decode — was written straight into the
// envelope with the developer's words in it, and that half was left behind when
// the backend half was fixed: a consumer was still reading
// `forbidden: missing permission tasks.TaskLookupQuery.CountTasksPerCategory`
// in a browser, which is the report that started the whole thing
// (docs/todos/one-error-string-serves-two-audiences.md).
//
// The rule is the same on both halves and it is stated once, here: a string
// written for an operator goes to observability, and the client gets a `code`
// to dispatch on plus a sentence from the catalog. Nothing reaches a reader
// unless somebody wrote it for a reader.

// MsgidMalformedValue is the sentence for one input the gateway could not read
// at all — a path or query param that is not the shape its type needs.
//
// It carries no `got` and no parser prose on purpose. `strconv.ParseInt:
// parsing "abc": invalid syntax` tells a person which Go function was upset;
// the FIELD is the part they can act on, and it travels as `details[].field`
// where a form can highlight it.
const MsgidMalformedValue = "This value is not in the expected format."

// Vocabulary is every msgid this package puts in front of a person.
//
// Harvested into each project's `.po` next to grpcerr's and the validation
// defaults', so these sentences are translatable rather than permanently
// English. A msgid with no catalog entry falls back to itself.
func Vocabulary() []string { return []string{MsgidMalformedValue} }

// WriteRefusal writes the canonical envelope for a refusal the gateway itself
// decided.
//
// `operator` is the developer's string — which permission was missing, which
// decode failed. It reaches observability and nowhere else; the envelope says
// only as much as the code does.
func WriteRefusal(ctx context.Context, w http.ResponseWriter, c codes.Code, operator string) {
	reportByCode(ctx, c, status.Error(c, operator))
	WriteError(w, HTTPStatusFromGRPCCode(c), GRPCCodeName(c), i18n.T(ctx, grpcerr.UserMsgid(c), nil))
}

// WriteBadInput reports one named input the gateway could not read.
//
// The client gets INVALID_ARGUMENT, the code's sentence, and a `details` entry
// naming the field — enough to highlight it. The parser's own words go to the
// operator, which is the only reader they were ever written for.
//
// `field` is the name the CALLER used (the proto/JSON spelling that appears in
// the path or query string), not the Go field: a person cannot act on a name
// they never typed.
//
// `where` is the binding the value came from — "path param", "query param",
// "form field", "cursor". It goes to the operator only, and it is separate from
// `field` for a reason a test pins: one name can be bound from more than one
// place, and "query param album_id" sent an operator to the URL for a value
// that arrived in a multipart form.
func WriteBadInput(ctx context.Context, w http.ResponseWriter, where, field string, cause error) {
	msg := strings.TrimSpace(where + " " + field)
	if cause != nil {
		msg += ": " + cause.Error()
	}
	reportByCode(ctx, codes.InvalidArgument, status.Error(codes.InvalidArgument, msg))
	WriteErrorWithDetails(w, http.StatusBadRequest, "INVALID_ARGUMENT",
		i18n.T(ctx, grpcerr.UserMsgid(codes.InvalidArgument), nil),
		[]FieldError{{
			Field:   field,
			Code:    "INVALID_VALUE",
			Message: i18n.T(ctx, MsgidMalformedValue, nil),
		}})
}

// WriteInvalidRequest is WriteRefusal for a request the gateway could not read
// at all — a body that is not the declared media type, a multipart envelope
// that would not parse. Named rather than code-taking so a generated handler
// needs no `codes` import for it.
func WriteInvalidRequest(ctx context.Context, w http.ResponseWriter, operator string) {
	WriteRefusal(ctx, w, codes.InvalidArgument, operator)
}

// WriteInternal is WriteRefusal for the gateway's own failure. Same reason for
// being named: the generated call site stays import-free.
func WriteInternal(ctx context.Context, w http.ResponseWriter, operator string) {
	WriteRefusal(ctx, w, codes.Internal, operator)
}

// WriteForbiddenCtx writes the canonical 403 without telling the caller which
// permission they lack.
//
// `perm` used to be the message. Naming it handed an unauthenticated prober the
// permission catalog one request at a time, and it is prose a person cannot act
// on either way — the answer to "why was I refused" is not a permission id.
// Operators need it on every refusal, so it goes to them on every refusal.
func WriteForbiddenCtx(ctx context.Context, w http.ResponseWriter, perm string) {
	operator := "forbidden"
	if perm != "" {
		operator = "forbidden: missing permission " + perm
	}
	WriteRefusal(ctx, w, codes.PermissionDenied, operator)
}

// WriteUnauthorizedCtx writes the canonical 401 without naming the credential
// scheme the surface refused.
//
// The scheme is the caller's own input, but "no auth method handles credential
// scheme BEARER" is a sentence about this gateway's configuration, and a
// caller holding a bearer token cannot do anything with it. It goes to the
// operator, who can.
func WriteUnauthorizedCtx(ctx context.Context, w http.ResponseWriter, scheme string) {
	operator := "no auth method handles this credential scheme"
	if scheme != "" {
		operator = "no auth method handles credential scheme " + scheme
	}
	WriteRefusal(ctx, w, codes.Unauthenticated, operator)
}
