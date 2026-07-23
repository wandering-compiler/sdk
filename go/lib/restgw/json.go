// Package restgw is the runtime library imported by every
// generated REST gateway binary. Hosts JSON ↔ proto helpers,
// gRPC error → HTTP status mapping, and (future iteration)
// the compiled custom JSON parser the user roadmap calls
// out for performance optimisation. Adapted from
// `github.com/mrs1lentcz/protobridge/runtime` per user
// direction 2026-05-03 ("zkopirovat … rozsirovat").
//
// The generator emits Go that imports this package directly;
// every cross-cutting runtime concern shared by every
// gateway handler lives here, not duplicated in the emit
// templates.
package restgw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// readBufPool recycles the request-body read buffer across calls so
// DecodeRequest doesn't allocate a fresh growing buffer per request
// (io.ReadAll showed up as ~12% of the round-trip's allocations).
// Safe to recycle: proto.Unmarshal / protojson copy the bytes they
// retain into the message, so the buffer holds no live references
// once Decode returns.
var readBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// pbMarshalBufPool recycles the binary-protobuf response buffer.
// Stored as *[]byte (not []byte) so Put doesn't box the slice header
// into a fresh allocation. Returned to the pool only after w.Write
// completes — net/http copies the bytes into the transport buffer
// synchronously, so the slice carries no live reference afterward.
var pbMarshalBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// ctJSON / ctProtobuf are shared, immutable Content-Type header
// value slices. Assigning them directly to the (already-canonical)
// "Content-Type" key skips http.Header.Set, which allocates a fresh
// []string{value} on every response (~18% of the PB round-trip's
// allocations). net/http only READS header values when writing the
// response, so sharing one slice across all responses is safe.
var (
	ctJSON     = []string{MIMEJSON}
	ctProtobuf = []string{MIMEProtobuf}
)

// Marshaller / Unmarshaller carry the project-wide proto-JSON
// settings. Exported via package-level vars so generated code
// + helpers below share one configuration source — operators
// who need different JSON shapes (different default-value
// emission, name casing) flip the var at process startup, not
// per-request.
//
// Defaults:
//   - EmitDefaultValues: false   — zero-valued fields drop out
//     of the response (smaller payloads, predictable diffs).
//   - UseProtoNames: true        — JSON keys match proto field
//     names (snake_case), not Go-style camelCase.
//   - DiscardUnknown: true       — unknown JSON fields don't
//     fail the request (forward-compat for evolving APIs).
var (
	Marshaller = protojson.MarshalOptions{
		EmitDefaultValues: false,
		UseProtoNames:     true,
	}
	Unmarshaller = protojson.UnmarshalOptions{
		DiscardUnknown: true,
	}
)

// MaxRequestBodyBytes caps the request body DecodeRequest will read,
// bounding the memory a single request can consume (restgw-sec-2).
// 10 MiB is generous for JSON/proto request payloads; the generated
// main may override it from an env knob at startup.
var MaxRequestBodyBytes int64 = 10 << 20

// DecodeRequest reads the HTTP request body and unmarshals
// it into a proto message. Empty / whitespace-only / `null`
// bodies leave the message untouched and return nil — HTTP
// bodies routinely carry trailing newlines, and some clients
// pad bare `null` with whitespace.
//
// The body format is negotiated from Content-Type (C9): a
// protobuf media type decodes via binary proto.Unmarshal,
// everything else (the default) via the JSON path. The
// REST-alias rewrite is JSON-only — the binary wire keys on
// field numbers, not names, so aliases don't apply there.
func DecodeRequest(r *http.Request, msg proto.Message) error {
	buf := readBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer readBufPool.Put(buf)
	// Bound the read so an unbounded body can't exhaust memory
	// (restgw-sec-2). Read one extra byte to detect overflow.
	n, err := buf.ReadFrom(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading request body: %w", err)
	}
	if n > MaxRequestBodyBytes {
		return fmt.Errorf("request body exceeds the %d-byte limit", MaxRequestBodyBytes)
	}
	body := buf.Bytes()
	if RequestFormat(r) == WireProto {
		if len(body) == 0 {
			return nil
		}
		if err := proto.Unmarshal(body, msg); err != nil {
			return fmt.Errorf("invalid protobuf: %w", err)
		}
		return nil
	}
	return UnmarshalProto(body, msg)
}

// UnmarshalProto decodes JSON bytes into a proto message
// using the runtime's standard options. Empty /
// whitespace-only / `null` input is a no-op (returns nil
// without modifying msg). Non-HTTP transports (future MCP /
// WebSocket variants) call this directly so they share
// decoding semantics with the REST gateway.
//
// When the message's descriptor tree carries any
// `(w17.field).rest_alias` annotation (G3i3-Misc-A), the
// helper accepts BOTH the alias and the proto name on the
// wire — alias keys get rewritten back to proto names before
// the protojson decode. Messages without aliases bypass the
// rewrite path entirely.
func UnmarshalProto(data []byte, msg proto.Message) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	payload := data
	if msg != nil {
		desc := msg.ProtoReflect().Descriptor()
		// Expand the collapsed-oneof dialect back to protojson's
		// flat shape BEFORE the alias restore (so inner variant
		// fields, still possibly aliased, are normalised next).
		if hasAnyOneof(desc) {
			if expanded, err := expandOneofsOnRequestJSON(payload, desc); err == nil {
				payload = expanded
			}
		}
		if hasAnyAlias(desc) {
			restored, err := restoreAliasesOnRequestJSON(payload, desc)
			if err != nil {
				// B27-restgw-1: the helper returns an error ONLY for the
				// alias+canonical collision (Q66-restgw-1) — malformed JSON
				// returns a nil error so protojson surfaces the parse error
				// below. So a non-nil error here is the ambiguous-request case
				// and MUST be surfaced; swallowing it let protojson silently
				// DiscardUnknown one of the two keys (a partial success on an
				// ambiguous body).
				return fmt.Errorf("invalid JSON: %w", err)
			}
			payload = restored
		}
	}
	if err := Unmarshaller.Unmarshal(payload, msg); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// MarshalProto marshals a proto message to JSON using the
// runtime's standard options. Every response path goes
// through this helper so JSON output stays consistent
// regardless of transport.
//
// When the message's descriptor tree carries any
// `(w17.field).rest_alias` annotation (G3i3-Misc-A), the
// helper post-processes the output to rename the aliased
// keys. Cached predicate (`hasAnyAlias`) means messages
// without aliases bypass the rewrite entirely; the byte-
// for-byte output matches the pre-feature path.
func MarshalProto(msg proto.Message) ([]byte, error) {
	return MarshalProtoAppend(nil, msg)
}

// MarshalProtoAppend appends msg's w17-dialect JSON to dst and returns the
// grown slice — the allocation-friendly entry the SSE/REST/WS write paths
// use with a pooled buffer (true end-to-end zero-alloc for a message with a
// registered generated marshaler and no rest_alias). MarshalProto is the
// `dst == nil` convenience over it.
//
// Dispatch: a generated marshaler (if registered) emits the collapsed
// dialect straight into dst — no protojson, no reshape. Otherwise it falls
// back to protojson, then the descriptor-driven alias rewrite + oneof
// collapse (the reflective correctness path; each allocates).
func MarshalProtoAppend(dst []byte, msg proto.Message) ([]byte, error) {
	if msg == nil {
		raw, err := Marshaller.Marshal(msg)
		if err != nil {
			return dst, err
		}
		return append(dst, raw...), nil
	}
	desc := msg.ProtoReflect().Descriptor()
	if fn := lookupJSONMarshaler(desc); fn != nil {
		if !hasAnyAlias(desc) {
			return fn(dst, msg) // zero-alloc: append the collapsed dialect directly
		}
		// Aliased + registered (rare): marshal in isolation, rewrite, append.
		out, err := fn(nil, msg)
		if err != nil {
			return dst, err
		}
		out, err = rewriteAliasesOnResponseJSON(out, desc)
		if err != nil {
			return dst, err
		}
		return append(dst, out...), nil
	}
	// Reflective fallback. Alias rewrite first (descriptor-aligned, flat),
	// then collapse oneofs — collapse matches variant keys alias-aware via
	// wireKey, so the order composes.
	raw, err := Marshaller.Marshal(msg)
	if err != nil {
		return dst, err
	}
	if hasAnyAlias(desc) {
		raw, err = rewriteAliasesOnResponseJSON(raw, desc)
		if err != nil {
			return dst, err
		}
	}
	if hasAnyOneof(desc) {
		raw, err = collapseOneofsOnResponseJSON(raw, desc)
		if err != nil {
			return dst, err
		}
	}
	return append(dst, raw...), nil
}

// WriteJSON writes pre-marshaled JSON bytes as the HTTP
// response with the given status code. Caller is responsible
// for the marshaling — used by [WriteResponse] (proto path)
// and [WriteError] (error envelope path).
func WriteJSON(w http.ResponseWriter, status int, data []byte) {
	w.Header()["Content-Type"] = ctJSON // shared slice — see ctJSON
	w.WriteHeader(status)
	_, _ = w.Write(data) // client disconnects mid-write are normal — drop the error.
}

// WriteResponse marshals a proto message and writes it with the
// given HTTP status. The optional trailing request enables C9
// content negotiation: when present, the response format is
// negotiated from its Accept header (binary protobuf when asked,
// JSON otherwise); when omitted, the response is JSON. The arg is
// variadic (pass at most one) purely for backward compatibility —
// generated code that predates C9 calls `WriteResponse(w, status,
// msg)` and keeps the historical JSON behaviour unchanged; the C9
// gateway template passes the request as the trailing arg. Marshal
// failures (proto3 data corruption) surface as a 500 JSON envelope.
func WriteResponse(w http.ResponseWriter, status int, msg proto.Message, rOpt ...*http.Request) {
	writeNegotiated(w, responseFormatOpt(rOpt), status, msg)
}

// responseFormatOpt resolves the negotiated response format from
// the optional trailing request (JSON when absent / nil).
func responseFormatOpt(rOpt []*http.Request) WireFormat {
	if len(rOpt) > 0 && rOpt[0] != nil {
		return ResponseFormat(rOpt[0])
	}
	return WireJSON
}

// writeNegotiated encodes msg in the chosen WireFormat and writes
// it with the given status. JSON goes through MarshalProto (so
// REST aliases + proto-name casing apply); protobuf goes through
// the binary marshaler with the protobuf media type. Both surface
// marshal failures as a JSON 500 (errors are never negotiated).
func writeNegotiated(w http.ResponseWriter, format WireFormat, status int, msg proto.Message) {
	if format == WireProto {
		bp := pbMarshalBufPool.Get().(*[]byte)
		data, err := proto.MarshalOptions{}.MarshalAppend((*bp)[:0], msg)
		if err != nil {
			pbMarshalBufPool.Put(bp)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "failed to marshal response")
			return
		}
		w.Header()["Content-Type"] = ctProtobuf // shared slice — see ctProtobuf
		w.WriteHeader(status)
		_, _ = w.Write(data) // client disconnects mid-write are normal — drop the error.
		*bp = data           // keep the grown capacity for reuse
		pbMarshalBufPool.Put(bp)
		return
	}
	// JSON: marshal into a pooled buffer + write + release (mirrors the PB
	// branch). A registered generated marshaler appends straight in →
	// zero steady-state allocation; the reflective fallback still grows
	// the same recycled buffer.
	bp := jsonBufPool.Get().(*[]byte)
	data, err := MarshalProtoAppend((*bp)[:0], msg)
	if err != nil {
		*bp = data
		jsonBufPool.Put(bp)
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "failed to marshal response")
		return
	}
	w.Header()["Content-Type"] = ctJSON // shared slice — see ctJSON
	w.WriteHeader(status)
	_, _ = w.Write(data) // client disconnects mid-write are normal — drop the error.
	*bp = data
	jsonBufPool.Put(bp)
}

// WriteResponseFiltered marshals a proto message minus the
// fields named in `omitFields` (proto names — snake_case as
// declared in the .proto). Used by REST handlers whose method
// declares `(w17.http).response_omit_fields = [...]` to hide
// internal-only fields from the public REST surface while
// still returning them to other gRPC clients.
//
// Implementation clones the message via proto.Clone (so the
// caller's `resp` keeps its original fields populated for
// any downstream observability that scans the gRPC response
// post-handler), walks the descriptor by proto name, and
// calls Clear on each match. Unknown field names are silently
// ignored — the codegen warns at emit time when the name
// doesn't exist on the response descriptor.
//
// `omitFields` may be empty / nil (degenerates to plain
// WriteResponse) so callers don't need a per-call branch.
//
// Format is negotiated from the optional trailing request (C9) the
// same way WriteResponse does — the omitted fields are cleared on
// a clone before encoding in either wire format. The request arg
// is variadic for backward compatibility (see WriteResponse).
func WriteResponseFiltered(w http.ResponseWriter, status int, msg proto.Message, omitFields []string, rOpt ...*http.Request) {
	if len(omitFields) == 0 {
		WriteResponse(w, status, msg, rOpt...)
		return
	}
	clone := proto.Clone(msg)
	refl := clone.ProtoReflect()
	fds := refl.Descriptor().Fields()
	for _, name := range omitFields {
		if fd := fds.ByName(protoreflect.Name(name)); fd != nil {
			refl.Clear(fd)
		}
	}
	writeNegotiated(w, responseFormatOpt(rOpt), status, clone)
}

// FieldError carries one per-field validation failure inside
// the error envelope's `details` list. Generated REST handlers
// accumulate these during the validation prelude (G3-V emit)
// and ship the full set in a single 400 response so callers
// can highlight every offending field at once instead of
// fixing-then-resubmitting one error at a time.
//
// Shape mirrors the gRPC `*w17.ErrorDetail` (REV-031 Phase C-6)
// so REST + gRPC clients see one consistent envelope shape:
//
//	{"field": "email", "code": "INVALID_EMAIL",
//	 "message": "must be a valid email address"}
//
// `code` is the FE-vocabulary code from the validation
// defaults catalog — same vocabulary the storage Stage-1
// emit uses (REQUIRED_VIOLATION / INVALID_EMAIL /
// MAX_LEN_VIOLATION / …). FE dispatch logic written for
// gRPC clients works against REST clients without change.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// errorEnvelope is the on-the-wire shape every error response
// uses. Single envelope keeps clients agnostic of which gRPC
// service / RPC produced the error — they always see
// `{"error": {"code": "...", "message": "...", "details": [...]}}`
// with HTTP status matching the gRPC code per
// [HTTPStatusFromGRPCCode]. Details are omitted when empty so
// non-validation errors stay terse.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Details []FieldError `json:"details,omitempty"`
}

// WriteError writes a structured error response without
// per-field details. Thin wrapper over
// [WriteErrorWithDetails] — most error paths (gRPC error
// passthrough, body-decode failure, path-param parse failure,
// missing-header) carry one top-level message and no field
// breakdown.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteErrorWithDetails(w, status, code, message, nil)
}

// WriteErrorWithDetails writes a structured error response
// carrying per-field validation details. `code` is the
// canonical gRPC code name ("INVALID_ARGUMENT", "NOT_FOUND",
// ...) so clients can branch on it without parsing prose.
// `details` is `nil` (or empty) for non-validation errors and
// gets omitted from the JSON envelope.
func WriteErrorWithDetails(w http.ResponseWriter, status int, code, message string, details []FieldError) {
	body, _ := json.Marshal(errorEnvelope{Error: errorBody{
		Code:    code,
		Message: message,
		Details: details,
	}})
	WriteJSON(w, status, body)
}

// errorEnvelopeJSON marshals the standard envelope to bytes.
// Used by SSE / WS helpers to embed the same error shape as
// unary responses inside a streaming frame — keeps the wire
// format consistent across transports.
func errorEnvelopeJSON(code, message string, details []FieldError) []byte {
	body, _ := json.Marshal(errorEnvelope{Error: errorBody{
		Code:    code,
		Message: message,
		Details: details,
	}})
	return body
}

// wsErrorFrameJSON is errorEnvelopeJSON carrying the WS frame
// discriminator. On a WS socket a bare `{"error":…}` object is
// indistinguishable from a data frame that happens to have an `error`
// field, so the error envelope is tagged like every other JSON frame
// (see the wsKind constants in streaming.go). SSE keeps the plain
// envelope — there the `event: error` line already discriminates.
func wsErrorFrameJSON(code, message string, details []FieldError) []byte {
	body := make([]byte, 0, 128)
	body = append(body, wsKindError...)
	inner, _ := json.Marshal(errorBody{
		Code:    code,
		Message: message,
		Details: details,
	})
	body = append(body, inner...)
	return append(body, '}')
}
