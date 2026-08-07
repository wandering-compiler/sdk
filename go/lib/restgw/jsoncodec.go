package restgw

import (
	"encoding/base64"
	"math"
	"strconv"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Generated zero-allocation JSON marshalers (the perf layer of the w17
// JSON dialect — docs/specs/gateway/json-dialect.md §5). SSE only
// marshals, so the hot path is marshal: a generated per-message
// MarshalFunc appends the w17-dialect JSON straight into a pooled buffer
// — no protojson, no reflection, and it emits the collapsed oneof shape
// directly (no post-pass). Messages WITHOUT a registered marshaler fall
// back to the reflective protojson + collapse path in MarshalProto, so
// this layer is purely additive: registering more messages widens
// coverage without changing semantics.
//
// Registration happens from generated init() functions; Go runs package
// init single-threaded, so the registry needs no lock for writes. After
// init the map is read-only (lookups only), so concurrent reads are safe.

// JSONMarshalFunc appends a message's w17-dialect JSON to dst and returns
// the grown slice. The concrete generated function type-asserts m to its
// message type. Must emit proto field names (UseProtoNames parity); the
// REST alias rewrite, if any, runs afterwards in MarshalProto.
type JSONMarshalFunc func(dst []byte, m proto.Message) ([]byte, error)

var jsonMarshalers = map[protoreflect.FullName]JSONMarshalFunc{}

// RegisterJSONMarshaler records the generated marshaler for a message
// type. Called only from generated init() (single-threaded) — no lock.
func RegisterJSONMarshaler(name protoreflect.FullName, fn JSONMarshalFunc) {
	jsonMarshalers[name] = fn
}

// lookupJSONMarshaler returns the registered marshaler for desc, or nil.
func lookupJSONMarshaler(desc protoreflect.MessageDescriptor) JSONMarshalFunc {
	if desc == nil || len(jsonMarshalers) == 0 {
		return nil
	}
	return jsonMarshalers[desc.FullName()]
}

// jsonBufPool recycles the marshal scratch buffer so a generated
// marshaler appends with zero steady-state allocation (mirrors the PB
// path's pbMarshalBufPool). Stored as *[]byte so Put doesn't box the
// slice header.
var jsonBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// ---- leaf encoders ----
//
// Hand-written once, append-style, producing correct JSON that matches
// protojson's value encoding (so a generated marshaler's output is
// semantically identical): 64-bit ints + enums-by-name + bytes render as
// JSON strings, 32-bit ints/floats as numbers, NaN/±Inf as quoted
// strings. They are NOT byte-identical to protojson in field ORDER or
// whitespace — only per-value.

// AppendJSONString appends s as a JSON string literal, escaping per the
// JSON spec. Like protojson (and unlike encoding/json) it does NOT
// HTML-escape <, >, & — those pass through verbatim.
func AppendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default: // other control chars < 0x20 → \u00XX
			const hex = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// AppendJSONInt64 appends a 64-bit signed int as a QUOTED decimal string
// (protojson encodes 64-bit ints as strings for JS precision).
func AppendJSONInt64(dst []byte, v int64) []byte {
	dst = append(dst, '"')
	dst = strconv.AppendInt(dst, v, 10)
	return append(dst, '"')
}

// AppendJSONUint64 appends a 64-bit unsigned int as a quoted decimal.
func AppendJSONUint64(dst []byte, v uint64) []byte {
	dst = append(dst, '"')
	dst = strconv.AppendUint(dst, v, 10)
	return append(dst, '"')
}

// AppendJSONInt32 appends a 32-bit signed int as a bare JSON number.
func AppendJSONInt32(dst []byte, v int32) []byte {
	return strconv.AppendInt(dst, int64(v), 10)
}

// AppendJSONUint32 appends a 32-bit unsigned int as a bare JSON number.
func AppendJSONUint32(dst []byte, v uint32) []byte {
	return strconv.AppendUint(dst, uint64(v), 10)
}

// AppendJSONBool appends true/false.
func AppendJSONBool(dst []byte, v bool) []byte {
	if v {
		return append(dst, "true"...)
	}
	return append(dst, "false"...)
}

// AppendJSONFloat64 appends a double as a JSON number, or the quoted
// "NaN"/"Infinity"/"-Infinity" sentinels (proto JSON mapping).
func AppendJSONFloat64(dst []byte, v float64) []byte {
	switch {
	case math.IsNaN(v):
		return append(dst, `"NaN"`...)
	case math.IsInf(v, +1):
		return append(dst, `"Infinity"`...)
	case math.IsInf(v, -1):
		return append(dst, `"-Infinity"`...)
	}
	return strconv.AppendFloat(dst, v, 'g', -1, 64)
}

// AppendJSONFloat32 appends a float as a JSON number, NaN/±Inf quoted.
func AppendJSONFloat32(dst []byte, v float32) []byte {
	switch d := float64(v); {
	case math.IsNaN(d):
		return append(dst, `"NaN"`...)
	case math.IsInf(d, +1):
		return append(dst, `"Infinity"`...)
	case math.IsInf(d, -1):
		return append(dst, `"-Infinity"`...)
	}
	return strconv.AppendFloat(dst, float64(v), 'g', -1, 32)
}

// AppendTimestampJSON appends a google.protobuf.Timestamp as the proto-JSON
// RFC 3339 string ("2006-01-02T15:04:05[.fff]Z", fractional digits in
// groups of 3/6/9, omitted when zero). nil → JSON null.
func AppendTimestampJSON(dst []byte, ts *timestamppb.Timestamp) []byte {
	if ts == nil {
		return append(dst, "null"...)
	}
	t := ts.AsTime().UTC()
	dst = append(dst, '"')
	dst = t.AppendFormat(dst, "2006-01-02T15:04:05")
	if ns := t.Nanosecond(); ns != 0 {
		// protojson trims to 3/6/9 significant fractional digits.
		frac := [9]byte{}
		for i := 8; i >= 0; i-- {
			frac[i] = byte('0' + ns%10)
			ns /= 10
		}
		// protojson trims trailing zeros in groups of 3, leaving
		// 3/6/9 fractional digits — never an arbitrary length.
		n := 9
		for n > 3 && frac[n-1] == '0' && frac[n-2] == '0' && frac[n-3] == '0' {
			n -= 3
		}
		dst = append(dst, '.')
		dst = append(dst, frac[:n]...)
	}
	return append(dst, 'Z', '"')
}

// AppendDurationJSON appends a google.protobuf.Duration as the proto-JSON
// "<seconds>[.fff]s" string. nil → JSON null.
func AppendDurationJSON(dst []byte, d *durationpb.Duration) []byte {
	if d == nil {
		return append(dst, "null"...)
	}
	secs, nanos := d.GetSeconds(), d.GetNanos()
	dst = append(dst, '"')
	if secs < 0 || nanos < 0 {
		dst = append(dst, '-')
		secs, nanos = -secs, -nanos
	}
	dst = strconv.AppendInt(dst, secs, 10)
	if nanos != 0 {
		frac := [9]byte{}
		v := nanos
		for i := 8; i >= 0; i-- {
			frac[i] = byte('0' + v%10)
			v /= 10
		}
		// protojson trims trailing zeros in groups of 3, leaving
		// 3/6/9 fractional digits — never an arbitrary length.
		n := 9
		for n > 3 && frac[n-1] == '0' && frac[n-2] == '0' && frac[n-3] == '0' {
			n -= 3
		}
		dst = append(dst, '.')
		dst = append(dst, frac[:n]...)
	}
	return append(dst, 's', '"')
}

// AppendJSONBytes appends raw bytes as a base64 (std, padded) JSON string
// — protojson's bytes encoding.
func AppendJSONBytes(dst []byte, b []byte) []byte {
	dst = append(dst, '"')
	n := base64.StdEncoding.EncodedLen(len(b))
	// Grow once, encode in place.
	start := len(dst)
	for cap(dst)-start < n {
		dst = append(dst[:cap(dst)], 0)[:start]
	}
	dst = dst[:start+n]
	base64.StdEncoding.Encode(dst[start:], b)
	return append(dst, '"')
}
