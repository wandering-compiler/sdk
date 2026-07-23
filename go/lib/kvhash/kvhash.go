// Package kvhash provides marshal/unmarshal helpers between proto
// messages and a Redis HASH (HSET / HGETALL value map). G3-KV-04
// HASH layout uses one Redis HASH per entity, with each scalar
// proto field landing in its own hash field. The codegen body
// emit calls into this package — there is no per-table generated
// encoder.
//
// Wire contract (forever consumer commitment — Python/JS clients
// reading the hash directly depend on this):
//
//   - hash field name = proto field name (snake_case from the
//     .proto declaration). Distinct from JSON layout's protojson
//     camelCase; HASH consumers read field names directly so
//     proto-native snake_case is the natural surface.
//   - bool                     → "true" / "false"
//   - string                   → as-is
//   - int32/int64/sint*/sfixed → strconv decimal
//   - uint32/uint64/fixed      → strconv decimal (unsigned)
//   - float                    → strconv FormatFloat (g, -1, 32)
//   - double                   → strconv FormatFloat (g, -1, 64)
//   - bytes                    → base64.StdEncoding
//   - enum                     → value name (label), e.g.
//     "USER_ROLE_ADMIN"; UNSPECIFIED-
//     valued fields are emitted as their
//     label too so round-trip is exact.
//   - google.protobuf.Timestamp → RFC3339Nano (UTC)
//   - google.protobuf.Duration  → "<seconds>s" form
//     (e.g. "1.500000s") — protojson
//     parity, parseable by time.ParseDuration
//
// Refused at runtime (defense-in-depth — IR-build also rejects):
//
//   - nested message types other than Timestamp / Duration
//   - repeated fields
//   - map fields
//
// All three are the iter-2 design call's "flat schemas only"
// rule for HASH layout v1.
package kvhash

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MarshalEntity flattens m's scalar fields into a slice ready
// for go-redis' HSet variadic argument: alternating
// [field, value, field, value, …] of `any`. Plain proto3 scalar
// fields have no presence, so every one is emitted and its default
// round-trips as the zero value. Presence-bearing fields — the
// supported WKT message fields (Timestamp/Duration) and proto3-
// optional scalars (which live in a synthetic oneof) — are omitted
// when unset so the set/unset distinction survives the round-trip
// (see the omit guard below). Nested-message fields other than the
// supported WKTs (Timestamp, Duration) are an error.
func MarshalEntity(m proto.Message) ([]any, error) {
	if m == nil {
		return nil, fmt.Errorf("kvhash: nil message")
	}
	desc := m.ProtoReflect().Descriptor()
	rm := m.ProtoReflect()

	out := make([]any, 0, desc.Fields().Len()*2)
	for i := 0; i < desc.Fields().Len(); i++ {
		fd := desc.Fields().Get(i)
		if err := checkFlatField(fd); err != nil {
			return nil, err
		}
		// Omit any UNSET presence-bearing field so UnmarshalEntity reads it back
		// as absent (proto default) rather than a present zero. This covers the
		// supported WKT message fields (Timestamp/Duration carry presence) AND
		// proto3-optional scalars, which live in a synthetic oneof and likewise
		// have presence — encoding an unset optional as its zero would conflate
		// null with zero in the HASH layout, losing the set/unset distinction on
		// round-trip (Q7-kvhash-1). Plain proto3 scalars have no presence and are
		// always encoded (defaults round-trip as their zero values).
		if fd.HasPresence() && !rm.Has(fd) {
			continue
		}
		val, err := encodeField(fd, rm.Get(fd))
		// coverage-exempt: encodeField only errors on GroupKind or
		// non-WKT message fields, both of which checkFlatField (called
		// immediately above) already rejects — so this wrap is
		// unreachable via MarshalEntity and exists for defense in
		// depth. encodeField's error arms are covered directly.
		if err != nil {
			return nil, fmt.Errorf("kvhash: encode %s: %w", fd.Name(), err)
		}
		out = append(out, string(fd.Name()), val)
	}
	return out, nil
}

// UnmarshalEntity decodes a HGETALL result map into m. Missing
// keys leave the corresponding field at its proto default. Same
// flat-schema restrictions as MarshalEntity — descriptor walk
// refuses repeated / map / unsupported nested-message fields.
func UnmarshalEntity(hash map[string]string, m proto.Message) error {
	if m == nil {
		return fmt.Errorf("kvhash: nil message")
	}
	desc := m.ProtoReflect().Descriptor()
	rm := m.ProtoReflect()

	for i := 0; i < desc.Fields().Len(); i++ {
		fd := desc.Fields().Get(i)
		if err := checkFlatField(fd); err != nil {
			return err
		}
		raw, ok := hash[string(fd.Name())]
		if !ok {
			continue
		}
		val, err := decodeField(fd, raw)
		if err != nil {
			return fmt.Errorf("kvhash: decode %s: %w", fd.Name(), err)
		}
		rm.Set(fd, val)
	}
	return nil
}

func checkFlatField(fd protoreflect.FieldDescriptor) error {
	switch {
	case fd.IsList():
		return fmt.Errorf("kvhash: field %q is repeated — HASH layout supports flat schemas only", fd.Name())
	case fd.IsMap():
		return fmt.Errorf("kvhash: field %q is a map — HASH layout supports flat schemas only", fd.Name())
	case fd.Kind() == protoreflect.MessageKind:
		switch fd.Message().FullName() {
		case "google.protobuf.Timestamp", "google.protobuf.Duration":
			return nil
		default:
			return fmt.Errorf("kvhash: field %q is a nested message %q — HASH layout supports flat schemas only", fd.Name(), fd.Message().FullName())
		}
	case fd.Kind() == protoreflect.GroupKind:
		return fmt.Errorf("kvhash: field %q is a proto2 group — not supported", fd.Name())
	}
	return nil
}

func encodeField(fd protoreflect.FieldDescriptor, v protoreflect.Value) (string, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		if v.Bool() {
			return "true", nil
		}
		return "false", nil
	case protoreflect.StringKind:
		return v.String(), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return strconv.FormatInt(v.Int(), 10), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(v.Uint(), 10), nil
	case protoreflect.FloatKind:
		return strconv.FormatFloat(v.Float(), 'g', -1, 32), nil
	case protoreflect.DoubleKind:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64), nil
	case protoreflect.BytesKind:
		return base64.StdEncoding.EncodeToString(v.Bytes()), nil
	case protoreflect.EnumKind:
		num := v.Enum()
		evd := fd.Enum().Values().ByNumber(num)
		if evd == nil {
			// Unknown numeric — emit the number as a fallback so
			// round-trip is still exact when both sides see the
			// same enum descriptor.
			return strconv.FormatInt(int64(num), 10), nil
		}
		return string(evd.Name()), nil
	case protoreflect.MessageKind:
		// Only Timestamp / Duration reach here per checkFlatField.
		msg := v.Message().Interface()
		switch m := msg.(type) {
		case *timestamppb.Timestamp:
			t := m.AsTime().UTC()
			return t.Format(time.RFC3339Nano), nil
		case *durationpb.Duration:
			d := m.AsDuration()
			// protojson Duration form is "<seconds>s" with up to
			// nine fractional digits. time.Duration.String emits
			// composite ("1m2s") which Redis consumers can't parse
			// the same way; render explicit float seconds + "s".
			seconds := float64(d) / float64(time.Second)
			return strconv.FormatFloat(seconds, 'f', -1, 64) + "s", nil
		default:
			return "", fmt.Errorf("unsupported well-known type %T", msg)
		}
	}
	return "", fmt.Errorf("unsupported kind %s", fd.Kind())
}

func decodeField(fd protoreflect.FieldDescriptor, raw string) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		switch raw {
		case "true":
			return protoreflect.ValueOfBool(true), nil
		case "false":
			return protoreflect.ValueOfBool(false), nil
		default:
			return protoreflect.Value{}, fmt.Errorf("invalid bool %q (expected \"true\" or \"false\")", raw)
		}
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(raw), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid int32 %q: %w", raw, err)
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid int64 %q: %w", raw, err)
		}
		return protoreflect.ValueOfInt64(n), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid uint32 %q: %w", raw, err)
		}
		return protoreflect.ValueOfUint32(uint32(n)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid uint64 %q: %w", raw, err)
		}
		return protoreflect.ValueOfUint64(n), nil
	case protoreflect.FloatKind:
		f, err := strconv.ParseFloat(raw, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid float %q: %w", raw, err)
		}
		return protoreflect.ValueOfFloat32(float32(f)), nil
	case protoreflect.DoubleKind:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid double %q: %w", raw, err)
		}
		return protoreflect.ValueOfFloat64(f), nil
	case protoreflect.BytesKind:
		b, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid base64 bytes %q: %w", raw, err)
		}
		return protoreflect.ValueOfBytes(b), nil
	case protoreflect.EnumKind:
		// Try label first (the canonical form); fall back to
		// numeric for forward-compat with older writers.
		if evd := fd.Enum().Values().ByName(protoreflect.Name(raw)); evd != nil {
			return protoreflect.ValueOfEnum(evd.Number()), nil
		}
		if n, err := strconv.ParseInt(raw, 10, 32); err == nil {
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), nil
		}
		return protoreflect.Value{}, fmt.Errorf("invalid enum value %q for %s", raw, fd.Enum().FullName())
	case protoreflect.MessageKind:
		switch fd.Message().FullName() {
		case "google.protobuf.Timestamp":
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return protoreflect.Value{}, fmt.Errorf("invalid RFC3339Nano timestamp %q: %w", raw, err)
			}
			ts := timestamppb.New(t)
			return protoreflect.ValueOfMessage(ts.ProtoReflect()), nil
		case "google.protobuf.Duration":
			// Accept the protojson "<n>s" form. time.ParseDuration
			// chokes on the bare-seconds form for fractional parts
			// > 9 digits, but the encoder uses '-1' precision so we
			// stay within range.
			d, err := time.ParseDuration(raw)
			if err != nil {
				return protoreflect.Value{}, fmt.Errorf("invalid duration %q: %w", raw, err)
			}
			dp := durationpb.New(d)
			return protoreflect.ValueOfMessage(dp.ProtoReflect()), nil
		}
	}
	return protoreflect.Value{}, fmt.Errorf("unsupported kind %s", fd.Kind())
}
