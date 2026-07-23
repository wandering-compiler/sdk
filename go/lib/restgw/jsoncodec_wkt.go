package restgw

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// Dynamic well-known-type JSON encoders for the generated marshaler
// (S2b inc5). These are the WKTs whose JSON is NOT a simple scalar:
// Struct/Value/ListValue (arbitrary dynamic JSON), FieldMask (a
// comma-joined camelCase path string), and Any (a `@type`-tagged object).
// Hand-written here so the generated per-message marshaler can emit a
// field of any of these inline — no protojson on the message itself.

// AppendStructJSON appends a google.protobuf.Struct as its JSON object.
// nil → "null". Uses encoding/json over the decoded Go map (the Struct's
// value IS arbitrary JSON, so this is exact).
func AppendStructJSON(dst []byte, s *structpb.Struct) []byte {
	if s == nil {
		return append(dst, "null"...)
	}
	b, err := json.Marshal(s.AsMap())
	if err != nil {
		return append(dst, "null"...)
	}
	return append(dst, b...)
}

// AppendValueJSON appends a google.protobuf.Value as its dynamic JSON
// value (null / number / string / bool / object / array).
func AppendValueJSON(dst []byte, v *structpb.Value) []byte {
	if v == nil {
		return append(dst, "null"...)
	}
	b, err := json.Marshal(v.AsInterface())
	if err != nil {
		return append(dst, "null"...)
	}
	return append(dst, b...)
}

// AppendListValueJSON appends a google.protobuf.ListValue as a JSON array.
func AppendListValueJSON(dst []byte, lv *structpb.ListValue) []byte {
	if lv == nil {
		return append(dst, "null"...)
	}
	b, err := json.Marshal(lv.AsSlice())
	if err != nil {
		return append(dst, "null"...)
	}
	return append(dst, b...)
}

// AppendFieldMaskJSON appends a google.protobuf.FieldMask as the proto-JSON
// string: paths joined by ",", each path's snake_case segments rendered
// lowerCamelCase. nil → "null".
func AppendFieldMaskJSON(dst []byte, fm *fieldmaskpb.FieldMask) []byte {
	if fm == nil {
		return append(dst, "null"...)
	}
	dst = append(dst, '"')
	for i, p := range fm.GetPaths() {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendCamelPath(dst, p)
	}
	return append(dst, '"')
}

// appendCamelPath lowerCamelCases each dot-separated segment of a
// FieldMask path (snake_case → lowerCamel), keeping the dots.
func appendCamelPath(dst []byte, path string) []byte {
	seg := 0
	for i := 0; i <= len(path); i++ {
		if i == len(path) || path[i] == '.' {
			dst = appendLowerCamel(dst, path[seg:i])
			if i < len(path) {
				dst = append(dst, '.')
			}
			seg = i + 1
		}
	}
	return dst
}

func appendLowerCamel(dst []byte, s string) []byte {
	up := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' {
			up = true
			continue
		}
		if up && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		up = false
		dst = append(dst, c)
	}
	return dst
}

// AppendAnyJSON appends a google.protobuf.Any as its proto-JSON object:
// `{"@type": <url>, ...}` inlining the resolved message's fields, or
// `{"@type": <url>, "value": <json>}` when the wrapped type has a custom
// JSON form (Timestamp/Duration/Struct/wrappers/…). nil → "null". The
// wrapped type is resolved against the process's global type registry —
// the target gateway has its own message types registered — and its
// fields are rendered by recursing into MarshalProtoAppend (generated or
// reflective). Returns an error when the type URL can't be resolved.
func AppendAnyJSON(dst []byte, a *anypb.Any) ([]byte, error) {
	if a == nil {
		return append(dst, "null"...), nil
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByURL(a.GetTypeUrl())
	if err != nil {
		return dst, fmt.Errorf("any: resolve %q: %w", a.GetTypeUrl(), err)
	}
	inner := mt.New().Interface()
	if err := proto.Unmarshal(a.GetValue(), inner); err != nil {
		return dst, fmt.Errorf("any: unmarshal %q: %w", a.GetTypeUrl(), err)
	}
	innerJSON, err := MarshalProtoAppend(nil, inner)
	if err != nil {
		return dst, fmt.Errorf("any: marshal inner %q: %w", a.GetTypeUrl(), err)
	}
	dst = append(dst, `{"@type":`...)
	dst = AppendJSONString(dst, a.GetTypeUrl())
	if isCustomJSONWKT(inner.ProtoReflect().Descriptor().FullName()) {
		// WKT with a non-object JSON form → wrap under "value".
		dst = append(dst, `,"value":`...)
		dst = append(dst, innerJSON...)
		return append(dst, '}'), nil
	}
	// Regular message → splice the @type into its object. innerJSON is
	// "{...}"; drop its leading "{", keep a comma when it has fields.
	if len(innerJSON) >= 2 && innerJSON[0] == '{' {
		if len(innerJSON) > 2 { // non-empty object
			dst = append(dst, ',')
		}
		dst = append(dst, innerJSON[1:]...)
		return dst, nil
	}
	// Defensive: inner didn't marshal to an object (shouldn't happen for a
	// non-custom message) — emit just the @type object.
	return append(dst, '}'), nil
}

// isCustomJSONWKT reports whether a message type renders as a non-object
// JSON form (so an Any wrapping it uses the `"value"` envelope). Struct
// and ListValue render as object/array but proto-JSON still value-wraps
// them inside Any, so they count here too.
func isCustomJSONWKT(name protoreflect.FullName) bool {
	switch name {
	case "google.protobuf.Timestamp", "google.protobuf.Duration",
		"google.protobuf.FieldMask", "google.protobuf.Struct",
		"google.protobuf.Value", "google.protobuf.ListValue",
		"google.protobuf.BoolValue", "google.protobuf.StringValue",
		"google.protobuf.BytesValue", "google.protobuf.Int32Value",
		"google.protobuf.Int64Value", "google.protobuf.UInt32Value",
		"google.protobuf.UInt64Value", "google.protobuf.FloatValue",
		"google.protobuf.DoubleValue":
		return true
	}
	return false
}
