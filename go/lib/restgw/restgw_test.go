package restgw_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// DecodeRequest round-trips JSON → proto using the runtime's
// standard options. wrapperspb.StringValue's canonical JSON
// encoding is the bare value (`"hello"`, not `{"value":
// "hello"}`) — protojson treats the well-known wrapper types
// specially.
func TestDecodeRequest_RoundTrip(t *testing.T) {
	body := strings.NewReader(`"hello"`)
	req := httptest.NewRequest(http.MethodPost, "/x", body)

	got := &wrapperspb.StringValue{}
	if err := restgw.DecodeRequest(req, got); err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if got.GetValue() != "hello" {
		t.Errorf("Value = %q, want hello", got.GetValue())
	}
}

// Empty / `null` / whitespace bodies are no-ops — message
// stays at zero value, no error returned.
func TestDecodeRequest_EmptyNoop(t *testing.T) {
	for _, body := range []string{"", "   ", "null", "  null  \n"} {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		got := &wrapperspb.StringValue{}
		if err := restgw.DecodeRequest(req, got); err != nil {
			t.Errorf("body=%q: unexpected err: %v", body, err)
		}
		if got.GetValue() != "" {
			t.Errorf("body=%q: Value should stay zero, got %q", body, got.GetValue())
		}
	}
}

// Malformed JSON surfaces as an `invalid JSON` error wrap.
func TestDecodeRequest_BadJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{not valid"))
	err := restgw.DecodeRequest(req, &wrapperspb.StringValue{})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("expected invalid-JSON err; got %v", err)
	}
}

// WriteResponse marshals + writes with the requested status.
func TestWriteResponse_HappyPath(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteResponse(rec, http.StatusCreated, wrapperspb.String("ok"), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body, _ := io.ReadAll(rec.Body)
	if !bytes.Contains(body, []byte(`"ok"`)) {
		t.Errorf("body should contain \"ok\"; got %s", body)
	}
}

// WriteError emits the canonical envelope + status. The
// envelope is nested under `error` so client code can branch
// on `body.error.code` without colliding with successful
// payloads that may carry their own top-level fields.
func TestWriteError_Envelope(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteError(rec, http.StatusBadRequest, "INVALID_ARGUMENT", "bad input")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var env struct {
		Error struct {
			Code    string              `json:"code"`
			Message string              `json:"message"`
			Details []restgw.FieldError `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if env.Error.Code != "INVALID_ARGUMENT" || env.Error.Message != "bad input" {
		t.Errorf("envelope = %+v, want {INVALID_ARGUMENT, bad input}", env.Error)
	}
	// Details default to omitempty when nil; absent in the
	// flat WriteError path.
	if len(env.Error.Details) != 0 {
		t.Errorf("Details = %+v, want empty", env.Error.Details)
	}
}

// WriteErrorWithDetails carries per-field validation
// breakdown round-tripped through the envelope's `details`
// list — ordering preserved, both fields surfaced.
func TestWriteErrorWithDetails_Envelope(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteErrorWithDetails(rec, http.StatusBadRequest, "INVALID_ARGUMENT", "validation failed", []restgw.FieldError{
		{Field: "email", Message: "is required"},
		{Field: "password", Message: "must be at least 8 characters"},
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var env struct {
		Error struct {
			Code    string              `json:"code"`
			Message string              `json:"message"`
			Details []restgw.FieldError `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if env.Error.Code != "INVALID_ARGUMENT" || env.Error.Message != "validation failed" {
		t.Errorf("top-level = %+v", env.Error)
	}
	if len(env.Error.Details) != 2 {
		t.Fatalf("Details count = %d, want 2 (got %+v)", len(env.Error.Details), env.Error.Details)
	}
	if env.Error.Details[0].Field != "email" || env.Error.Details[0].Message != "is required" {
		t.Errorf("Details[0] = %+v", env.Error.Details[0])
	}
	if env.Error.Details[1].Field != "password" || env.Error.Details[1].Message != "must be at least 8 characters" {
		t.Errorf("Details[1] = %+v", env.Error.Details[1])
	}
}

// gRPC code → HTTP status mapping covers the canonical set.
// One sample per branch is enough — the helper is a flat
// switch.
func TestHTTPStatusFromGRPCCode(t *testing.T) {
	cases := []struct {
		code codes.Code
		want int
	}{
		// Happy + most-used.
		{codes.OK, http.StatusOK},
		{codes.InvalidArgument, http.StatusBadRequest},
		{codes.NotFound, http.StatusNotFound},
		{codes.AlreadyExists, http.StatusConflict},
		{codes.PermissionDenied, http.StatusForbidden},
		{codes.Unauthenticated, http.StatusUnauthorized},
		{codes.ResourceExhausted, http.StatusTooManyRequests},
		{codes.Unimplemented, http.StatusNotImplemented},
		{codes.Internal, http.StatusInternalServerError},
		{codes.Unavailable, http.StatusServiceUnavailable},
		// G3-T-01: round out the remaining canonical codes that
		// were uncovered. Each of these maps to a non-trivial HTTP
		// status that consumers check explicitly (504 retry hint,
		// 499 close-on-cancel, 409-on-Aborted optimistic-lock
		// retry, 400-on-FailedPrecondition for "wrong state").
		{codes.Canceled, 499},
		{codes.Unknown, http.StatusInternalServerError},
		{codes.DeadlineExceeded, http.StatusGatewayTimeout},
		{codes.Aborted, http.StatusConflict},
		{codes.FailedPrecondition, http.StatusBadRequest},
		{codes.OutOfRange, http.StatusBadRequest},
		{codes.DataLoss, http.StatusInternalServerError},
		// Default-fallthrough — synthesised non-canonical code
		// (caller passes a bogus int via codes.Code(N)).
		{codes.Code(99), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := restgw.HTTPStatusFromGRPCCode(c.code); got != c.want {
			t.Errorf("%s -> %d, want %d", c.code, got, c.want)
		}
	}
}

// G3-GW-06: WriteResponseFiltered clears named fields before
// marshal — internal-only fields stay populated on the source
// proto (so other gRPC clients still see them) but never
// reach the REST JSON output. FieldMask uses the well-known
// JSON form (comma-joined paths as a single string), so a
// cleared `paths` field marshals to the empty string `""`.
func TestWriteResponseFiltered_ClearsNamedFields(t *testing.T) {
	rec := httptest.NewRecorder()
	msg := &fieldmaskpb.FieldMask{Paths: []string{"a", "b", "c"}}

	restgw.WriteResponseFiltered(rec, http.StatusOK, msg, []string{"paths"}, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// FieldMask well-known form: cleared paths → empty-string
	// JSON. Pre-clearing it would have rendered `"a,b,c"`.
	if strings.Contains(body, "a") || strings.Contains(body, "b") || strings.Contains(body, "c") {
		t.Errorf("expected paths cleared from body; got %q", body)
	}
	// Source message untouched — caller observes original
	// state for any post-handler observability.
	if len(msg.Paths) != 3 {
		t.Errorf("source message mutated; len(Paths) = %d, want 3", len(msg.Paths))
	}
}

// G3-GW-06: empty omit list degenerates to plain WriteResponse —
// the field stays populated in the body. FieldMask renders the
// paths as a comma-joined string under the WKT form.
func TestWriteResponseFiltered_EmptyOmitList_PassThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	msg := &fieldmaskpb.FieldMask{Paths: []string{"x"}}

	restgw.WriteResponseFiltered(rec, http.StatusOK, msg, nil, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "x") {
		t.Errorf("expected `x` to remain when no omit list; got %q", body)
	}
}

// G3-GW-06: unknown field name is silently ignored — runtime
// match-by-name returns nil descriptor which the helper
// skips. Author typos surface at codegen time (parser
// validate); the runtime stays defensive.
func TestWriteResponseFiltered_UnknownField_NoOp(t *testing.T) {
	rec := httptest.NewRecorder()
	msg := &fieldmaskpb.FieldMask{Paths: []string{"x"}}

	restgw.WriteResponseFiltered(rec, http.StatusOK, msg, []string{"does_not_exist"}, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "x") {
		t.Errorf("expected `x` to remain (unknown field name no-op); got %q", body)
	}
}

// G3-T-01: WriteGRPCError(nil) — the defensive branch that
// catches a caller passing a nil error. Surfaces as 500
// `INTERNAL` with the diagnostic body so the consumer sees
// "something went wrong" instead of an empty-200 success.
func TestWriteGRPCError_NilErr(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteGRPCError(rec, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Errorf("body should carry INTERNAL code; got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nil error") {
		t.Errorf("body should carry the diagnostic message; got %s", rec.Body.String())
	}
}

// WriteGRPCError translates a gRPC status into the canonical
// envelope with matching HTTP status.
func TestWriteGRPCError_Status(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteGRPCError(rec, status.Error(codes.NotFound, "user 42 not found"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	// Code is the canonical UPPER_SNAKE name (NOT_FOUND), not
	// grpc's PascalCase codes.Code.String() ("NotFound").
	if !strings.Contains(rec.Body.String(), `"NOT_FOUND"`) {
		t.Errorf("body should carry code; got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "user 42 not found") {
		t.Errorf("body should carry message; got %s", rec.Body.String())
	}
}

// GRPCCodeName maps every gRPC code to its canonical
// UPPER_SNAKE name — the documented envelope `code` spelling.
// Regression guard: grpc's codes.Code.String() returns
// PascalCase ("InvalidArgument", "NotFound"), which would
// break clients that branch on the UPPER_SNAKE contract every
// other path in the package emits.
func TestGRPCCodeName_Canonical(t *testing.T) {
	cases := map[codes.Code]string{
		codes.OK:                 "OK",
		codes.Canceled:           "CANCELLED",
		codes.Unknown:            "UNKNOWN",
		codes.InvalidArgument:    "INVALID_ARGUMENT",
		codes.DeadlineExceeded:   "DEADLINE_EXCEEDED",
		codes.NotFound:           "NOT_FOUND",
		codes.AlreadyExists:      "ALREADY_EXISTS",
		codes.PermissionDenied:   "PERMISSION_DENIED",
		codes.ResourceExhausted:  "RESOURCE_EXHAUSTED",
		codes.FailedPrecondition: "FAILED_PRECONDITION",
		codes.Aborted:            "ABORTED",
		codes.OutOfRange:         "OUT_OF_RANGE",
		codes.Unimplemented:      "UNIMPLEMENTED",
		codes.Internal:           "INTERNAL",
		codes.Unavailable:        "UNAVAILABLE",
		codes.DataLoss:           "DATA_LOSS",
		codes.Unauthenticated:    "UNAUTHENTICATED",
	}
	for c, want := range cases {
		if got := restgw.GRPCCodeName(c); got != want {
			t.Errorf("GRPCCodeName(%v) = %q, want %q", c, got, want)
		}
	}
	// Unknown numeric code falls through to INTERNAL.
	if got := restgw.GRPCCodeName(codes.Code(99)); got != "INTERNAL" {
		t.Errorf("GRPCCodeName(99) = %q, want INTERNAL", got)
	}
}

// Non-status errors fall through to 500 INTERNAL.
func TestWriteGRPCError_PlainErr(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteGRPCError(rec, errors.New("boom"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Errorf("body should carry INTERNAL code; got %s", rec.Body.String())
	}
}

// SSE headers + one event = the standard `data: <json>\n\n`
// shape every browser EventSource consumer expects. Headers
// are flushed before any payload so clients see the channel
// open immediately.
func TestWriteSSEHeaders_AndEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher, ok := restgw.WriteSSEHeaders(rec)
	if !ok {
		t.Fatal("WriteSSEHeaders should accept httptest.ResponseRecorder")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	if err := restgw.WriteSSEEvent(rec, flusher, wrapperspb.String("hello")); err != nil {
		t.Fatalf("WriteSSEEvent: %v", err)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("expected `data: ` prefix; got %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("expected trailing blank line; got %q", body)
	}
	if !strings.Contains(body, `"hello"`) {
		t.Errorf("payload should contain \"hello\"; got %q", body)
	}
}

// Two events back-to-back land as two `data: <json>\n\n`
// frames in source order; the SSE reader on the client side
// splits on the blank-line delimiter.
func TestWriteSSEEvent_MultipleFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher, _ := restgw.WriteSSEHeaders(rec)

	for _, v := range []string{"a", "b", "c"} {
		if err := restgw.WriteSSEEvent(rec, flusher, wrapperspb.String(v)); err != nil {
			t.Fatalf("event %q: %v", v, err)
		}
	}
	body := rec.Body.String()
	frames := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	if len(frames) != 3 {
		t.Fatalf("frame count = %d, want 3 (body=%q)", len(frames), body)
	}
	for i, want := range []string{`"a"`, `"b"`, `"c"`} {
		if !strings.Contains(frames[i], want) {
			t.Errorf("frame[%d] = %q, missing %s", i, frames[i], want)
		}
	}
}

// gRPC status errors translate to the SSE error envelope —
// canonical code name + human message.
func TestWriteSSEGRPCError_Status(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher, _ := restgw.WriteSSEHeaders(rec)
	restgw.WriteSSEGRPCError(rec, flusher, status.Error(codes.NotFound, "missing"))

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("expected `event: error`; got %q", body)
	}
	if !strings.Contains(body, `"NOT_FOUND"`) {
		t.Errorf("expected NOT_FOUND code; got %q", body)
	}
	if !strings.Contains(body, "missing") {
		t.Errorf("expected message; got %q", body)
	}
}
