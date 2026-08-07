// Package datamigrate — protobuf encoding (Phase E v2.2 —
// D-iter3-19).
//
// **Why dynamicpb.** Apply-tool runs deploy-time against KV
// stores that hold proto-encoded values. The compiler can't
// generate Go for every project's User/Account/Order message,
// so the migration body ships the relevant `FileDescriptorSet`
// bytes inline (base64 in the YAML `proto_descriptor` field)
// + the message FQN (`proto_message`). At apply time we build
// a `protoreflect.MessageType` via `dynamicpb.NewMessageType`
// and reflectively unmarshal → mutate → marshal each value.
//
// **Scope (v2.2).** Scalar field kinds only (string / int32 /
// int64 / uint32 / uint64 / bool / float / double / bytes).
// Repeated fields, map fields, message-typed fields, and
// enum-typed fields are rejected with an explicit "v2.2: not
// supported" error so the producer / operator sees what's
// missing. Adding kinds is incremental — each one is one new
// case in `setFieldFromYAML`.
//
// **What's NOT here (yet).** Producer-side emission of
// `encoding: protobuf` migrations. The compiler today doesn't
// have a way to declare "table X is stored as proto message Y
// in KV"; that's iter-2 storage-layer territory. v2.2 ships
// the consumer so manually-authored YAML data migrations or
// future emit/* changes Just Work.
package datamigrate

import (
	"encoding/base64"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// ProtoCodec is the dynamicpb-based encoder/decoder used by
// encoding=protobuf data migrations. Built once per migration
// from the inline `FileDescriptorSet` + message FQN, then
// invoked per-key by the dialect Applier via ApplyOp.
//
// Construction is lossy if the descriptor set + message FQN
// don't resolve — callers MUST treat NewProtoCodec errors as
// migration-refusal: re-running with the same body would fail
// the same way, so propagating the error up is the right
// posture.
type ProtoCodec struct {
	desc protoreflect.MessageDescriptor
	typ  protoreflect.MessageType
}

// NewProtoCodec parses a serialised FileDescriptorSet and
// builds a codec for the message with FQN `messageFQN`.
//
// Inputs:
//
//   - `fdsBytes` is the raw `FileDescriptorSet` proto-wire
//     bytes (caller has already base64-decoded the YAML
//     `proto_descriptor` field).
//   - `messageFQN` is the fully-qualified message name (e.g.
//     `pkg.Subpkg.User`). Must resolve to a message
//     descriptor inside the supplied set.
//
// Returns errors with operator-readable context for the four
// failure modes: malformed FDS bytes, descriptor build
// failure, unknown message FQN, and "FQN resolves but isn't
// a message" (caller pointed at an enum / file / service).
func NewProtoCodec(fdsBytes []byte, messageFQN string) (*ProtoCodec, error) {
	if len(fdsBytes) == 0 {
		return nil, fmt.Errorf("proto_descriptor is empty")
	}
	if messageFQN == "" {
		return nil, fmt.Errorf("proto_message is empty")
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(fdsBytes, &fds); err != nil {
		return nil, fmt.Errorf("decode proto_descriptor: %w", err)
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return nil, fmt.Errorf("build descriptor registry: %w", err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(messageFQN))
	if err != nil {
		return nil, fmt.Errorf("proto_message %q: %w", messageFQN, err)
	}
	msgDesc, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("proto_message %q resolves to a %T, want message",
			messageFQN, d)
	}
	return &ProtoCodec{
		desc: msgDesc,
		typ:  dynamicpb.NewMessageType(msgDesc),
	}, nil
}

// DecodeProtoDescriptor base64-decodes a YAML `proto_descriptor`
// string into the raw `FileDescriptorSet` bytes NewProtoCodec
// expects. Caller convenience — keeps the base64 detail out of
// dialect Appliers.
func DecodeProtoDescriptor(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, fmt.Errorf("proto_descriptor is empty")
	}
	out, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("base64-decode proto_descriptor: %w", err)
	}
	return out, nil
}

// ApplyOp transforms one proto-encoded value by applying the
// field-level Operation. Mirrors JSONApplyOp's contract:
//
//   - `raw` is the existing wire-format proto bytes from the
//     KV store. Empty bytes short-circuit (nil, false, nil).
//   - Returns (newRaw, changed, err); newRaw is non-nil only
//     when changed=true.
//
// Field resolution: ADD_FIELD_DEFAULT / REMOVE_FIELD use
// `op.Field` as the proto field name; RENAME_FIELD uses
// `op.From` / `op.To`. Fields are looked up by name on the
// codec's MessageDescriptor — unknown field names error.
//
// Replay safety: presence checks via `protoreflect.Message.Has`
// mean ADD only sets when missing, REMOVE only clears when
// present, RENAME only acts when From is present. Re-running
// on an already-mutated value returns (nil, false, nil).
func (c *ProtoCodec) ApplyOp(raw []byte, op Operation) ([]byte, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	msg := dynamicpb.NewMessage(c.desc)
	if err := proto.Unmarshal(raw, msg); err != nil {
		return nil, false, fmt.Errorf("decode proto: %w", err)
	}

	changed := false
	switch op.Op {
	case OpAddFieldDefault:
		fd := c.desc.Fields().ByName(protoreflect.Name(op.Field))
		if fd == nil {
			return nil, false, fmt.Errorf("unknown field %q on %s",
				op.Field, c.desc.FullName())
		}
		if !msg.Has(fd) {
			val, err := scalarValue(fd, op.Value)
			if err != nil {
				return nil, false, fmt.Errorf("ADD_FIELD_DEFAULT %s: %w", op.Field, err)
			}
			msg.Set(fd, val)
			changed = true
		}
	case OpRemoveField:
		fd := c.desc.Fields().ByName(protoreflect.Name(op.Field))
		if fd == nil {
			return nil, false, fmt.Errorf("unknown field %q on %s",
				op.Field, c.desc.FullName())
		}
		if msg.Has(fd) {
			msg.Clear(fd)
			changed = true
		}
	case OpRenameField:
		fromFD := c.desc.Fields().ByName(protoreflect.Name(op.From))
		if fromFD == nil {
			return nil, false, fmt.Errorf("unknown field %q on %s",
				op.From, c.desc.FullName())
		}
		toFD := c.desc.Fields().ByName(protoreflect.Name(op.To))
		if toFD == nil {
			return nil, false, fmt.Errorf("unknown field %q on %s",
				op.To, c.desc.FullName())
		}
		if msg.Has(fromFD) {
			// Shape guard: dynamicpb.Message.Set panics ("invalid
			// value type") when From and To differ in kind /
			// cardinality (string→int, singular→repeated,
			// message→scalar). validate-time can't catch it — it
			// has no descriptor access — so reject incompatible
			// shapes here with a clean error instead of panicking a
			// deploy-time apply goroutine mid-migration. Type check
			// first: a shape mismatch makes the clobber comparison
			// below meaningless.
			if fromFD.Kind() != toFD.Kind() || fromFD.IsList() != toFD.IsList() || fromFD.IsMap() != toFD.IsMap() ||
				(isMessageKind(fromFD.Kind()) && fromFD.Message().FullName() != toFD.Message().FullName()) {
				return nil, false, fmt.Errorf(
					"RENAME_FIELD %q→%q: incompatible field types (%s→%s)",
					op.From, op.To, fieldShape(fromFD), fieldShape(toFD))
			}
			// Clobber guard: mirror JSONApplyOp — if To is already
			// present with a value distinct from From's, the rename
			// would overwrite and lose it. Abort so the migration
			// surfaces the conflict rather than silently corrupting
			// data. A destination holding the SAME value is benign
			// (clear source, stay idempotent).
			if msg.Has(toFD) && !msg.Get(fromFD).Equal(msg.Get(toFD)) {
				return nil, false, fmt.Errorf(
					"RENAME_FIELD %q→%q: destination field already present with a different value — refusing to overwrite", op.From, op.To)
			}
			msg.Set(toFD, msg.Get(fromFD))
			msg.Clear(fromFD)
			changed = true
		}
	default:
		return nil, false, fmt.Errorf("unsupported op %q", op.Op)
	}

	if !changed {
		return nil, false, nil
	}
	out, err := proto.Marshal(msg)
	if err != nil {
		return nil, false, fmt.Errorf("encode proto: %w", err)
	}
	return out, true, nil
}

// isMessageKind reports whether a field's kind carries a nested
// message descriptor (message or proto2 group) — the case where
// shape-compatibility also has to compare the message FQN, not
// just the kind enum.
func isMessageKind(k protoreflect.Kind) bool {
	return k == protoreflect.MessageKind || k == protoreflect.GroupKind
}

// fieldShape renders a field's kind + cardinality for RENAME's
// incompatible-types error. Message-typed fields print their FQN
// (the discriminating detail) rather than the bare "message" kind.
func fieldShape(fd protoreflect.FieldDescriptor) string {
	kind := fd.Kind().String()
	if isMessageKind(fd.Kind()) {
		kind = string(fd.Message().FullName())
	}
	switch {
	case fd.IsList():
		return "repeated " + kind
	case fd.IsMap():
		return "map " + kind
	}
	return kind
}

// scalarValue converts a YAML-decoded `any` to the matching
// `protoreflect.Value` for the field's kind. v2.2 covers
// scalar kinds (string / int32 / int64 / uint32 / uint64 /
// bool / float / double / bytes). Other kinds (enum, message,
// repeated, map) error with explicit version tags so future
// graduations are obvious.
//
// YAML's int decode is `int` (or `int64` on 64-bit platforms)
// and float decode is `float64`. Conversion is bounded so a
// user supplying an out-of-range value (e.g. `value: 99999999999999`
// on an int32 field) gets an explicit error instead of a
// silent overflow.
func scalarValue(fd protoreflect.FieldDescriptor, raw any) (protoreflect.Value, error) {
	if fd.IsList() {
		return protoreflect.Value{}, fmt.Errorf("repeated field kind unsupported in v2.2")
	}
	if fd.IsMap() {
		return protoreflect.Value{}, fmt.Errorf("map field kind unsupported in v2.2")
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		s, ok := raw.(string)
		if !ok {
			if raw == nil {
				return protoreflect.ValueOfString(""), nil
			}
			return protoreflect.Value{}, fmt.Errorf("expected string for kind=%s, got %T", fd.Kind(), raw)
		}
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BoolKind:
		b, ok := raw.(bool)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("expected bool for kind=%s, got %T", fd.Kind(), raw)
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := toInt64(raw)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("kind=%s: %w", fd.Kind(), err)
		}
		if n < -1<<31 || n > 1<<31-1 {
			return protoreflect.Value{}, fmt.Errorf("kind=%s: %d out of int32 range", fd.Kind(), n)
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := toInt64(raw)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("kind=%s: %w", fd.Kind(), err)
		}
		return protoreflect.ValueOfInt64(n), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := toUint64(raw)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("kind=%s: %w", fd.Kind(), err)
		}
		if n > 1<<32-1 {
			return protoreflect.Value{}, fmt.Errorf("kind=%s: %d out of uint32 range", fd.Kind(), n)
		}
		return protoreflect.ValueOfUint32(uint32(n)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := toUint64(raw)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("kind=%s: %w", fd.Kind(), err)
		}
		return protoreflect.ValueOfUint64(n), nil
	case protoreflect.FloatKind:
		f, err := toFloat64(raw)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("kind=%s: %w", fd.Kind(), err)
		}
		return protoreflect.ValueOfFloat32(float32(f)), nil
	case protoreflect.DoubleKind:
		f, err := toFloat64(raw)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("kind=%s: %w", fd.Kind(), err)
		}
		return protoreflect.ValueOfFloat64(f), nil
	case protoreflect.BytesKind:
		switch v := raw.(type) {
		case nil:
			return protoreflect.ValueOfBytes(nil), nil
		case []byte:
			return protoreflect.ValueOfBytes(v), nil
		case string:
			// YAML doesn't have a native bytes type; the convention
			// is to base64-encode in a string scalar.
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return protoreflect.Value{}, fmt.Errorf("kind=BytesKind: base64 decode: %w", err)
			}
			return protoreflect.ValueOfBytes(b), nil
		}
		return protoreflect.Value{}, fmt.Errorf("kind=BytesKind: expected string (base64), got %T", raw)
	case protoreflect.EnumKind:
		return protoreflect.Value{}, fmt.Errorf("enum field kind unsupported in v2.2")
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return protoreflect.Value{}, fmt.Errorf("message / group field kind unsupported in v2.2")
	}
	return protoreflect.Value{}, fmt.Errorf("unsupported field kind %s", fd.Kind())
}

// toInt64 normalises an `any` (yaml.v3 decode output) to int64.
// Accepts every integer width yaml.v3 might produce.
func toInt64(raw any) (int64, error) {
	switch v := raw.(type) {
	case int:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case uint:
		return int64(v), nil
	case uint8:
		return int64(v), nil
	case uint16:
		return int64(v), nil
	case uint32:
		return int64(v), nil
	case uint64:
		if v > 1<<63-1 {
			return 0, fmt.Errorf("uint64 %d overflows int64", v)
		}
		return int64(v), nil
	case nil:
		return 0, nil
	}
	return 0, fmt.Errorf("expected integer, got %T", raw)
}

// toUint64 normalises an `any` to uint64. Negative values
// error explicitly.
func toUint64(raw any) (uint64, error) {
	switch v := raw.(type) {
	case int:
		if v < 0 {
			return 0, fmt.Errorf("negative %d for unsigned field", v)
		}
		return uint64(v), nil
	case int64:
		if v < 0 {
			return 0, fmt.Errorf("negative %d for unsigned field", v)
		}
		return uint64(v), nil
	case uint:
		return uint64(v), nil
	case uint64:
		return v, nil
	case nil:
		return 0, nil
	}
	return 0, fmt.Errorf("expected unsigned integer, got %T", raw)
}

// toFloat64 normalises an `any` to float64. Accepts ints +
// floats; ints become exact float values where possible.
func toFloat64(raw any) (float64, error) {
	switch v := raw.(type) {
	case float32:
		return float64(v), nil
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case nil:
		return 0, nil
	}
	return 0, fmt.Errorf("expected number, got %T", raw)
}
