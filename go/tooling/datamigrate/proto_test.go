package datamigrate_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// fakeFDS hand-crafts a FileDescriptorSet for a synthetic
// `test.User { string id, string email, string full_name,
// int32 age, bool active, bytes payload }` message.
// Returns the serialised bytes NewProtoCodec consumes + a
// helper that builds raw wire bytes for a test User instance
// using the same descriptor.
func fakeFDS(t *testing.T) ([]byte, func(set map[string]any) []byte) {
	t.Helper()
	stringT := descriptorpb.FieldDescriptorProto_TYPE_STRING
	int32T := descriptorpb.FieldDescriptorProto_TYPE_INT32
	int64T := descriptorpb.FieldDescriptorProto_TYPE_INT64
	uint32T := descriptorpb.FieldDescriptorProto_TYPE_UINT32
	uint64T := descriptorpb.FieldDescriptorProto_TYPE_UINT64
	boolT := descriptorpb.FieldDescriptorProto_TYPE_BOOL
	bytesT := descriptorpb.FieldDescriptorProto_TYPE_BYTES
	doubleT := descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
	floatT := descriptorpb.FieldDescriptorProto_TYPE_FLOAT
	enumT := descriptorpb.FieldDescriptorProto_TYPE_ENUM
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED

	syntax := "proto3"
	pkg := "test"
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("test.proto"),
		Package: &pkg,
		Syntax:  &syntax,
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Status"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("UNKNOWN"), Number: proto.Int32(0)},
				{Name: proto.String("ACTIVE"), Number: proto.Int32(1)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("User"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Type: &stringT, Label: &optional},
				{Name: proto.String("email"), Number: proto.Int32(2), Type: &stringT, Label: &optional},
				{Name: proto.String("full_name"), Number: proto.Int32(3), Type: &stringT, Label: &optional},
				{Name: proto.String("age"), Number: proto.Int32(4), Type: &int32T, Label: &optional},
				{Name: proto.String("active"), Number: proto.Int32(5), Type: &boolT, Label: &optional},
				{Name: proto.String("payload"), Number: proto.Int32(6), Type: &bytesT, Label: &optional},
				{Name: proto.String("score"), Number: proto.Int32(7), Type: &doubleT, Label: &optional},
				{Name: proto.String("ratio"), Number: proto.Int32(8), Type: &floatT, Label: &optional},
				{Name: proto.String("level64"), Number: proto.Int32(9), Type: &int64T, Label: &optional},
				{Name: proto.String("count32"), Number: proto.Int32(10), Type: &uint32T, Label: &optional},
				{Name: proto.String("count64"), Number: proto.Int32(11), Type: &uint64T, Label: &optional},
				{Name: proto.String("status"), Number: proto.Int32(12), Type: &enumT, TypeName: proto.String(".test.Status"), Label: &optional},
				{Name: proto.String("tags"), Number: proto.Int32(13), Type: &stringT, Label: &repeated},
			},
		}},
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	fdsBytes, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal FileDescriptorSet: %v", err)
	}

	// Build a helper that produces wire bytes for a User by
	// using the same descriptor we just baked in.
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("build files: %v", err)
	}
	d, err := files.FindDescriptorByName("test.User")
	if err != nil {
		t.Fatalf("find User: %v", err)
	}
	userDesc := d.(protoreflect.MessageDescriptor)
	encode := func(set map[string]any) []byte {
		msg := dynamicpb.NewMessage(userDesc)
		for name, val := range set {
			fd := userDesc.Fields().ByName(protoreflect.Name(name))
			if fd == nil {
				t.Fatalf("encode helper: unknown field %q", name)
			}
			msg.Set(fd, valueOf(t, fd, val))
		}
		raw, err := proto.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal User: %v", err)
		}
		return raw
	}
	return fdsBytes, encode
}

func valueOf(t *testing.T, fd protoreflect.FieldDescriptor, v any) protoreflect.Value {
	t.Helper()
	switch v := v.(type) {
	case string:
		return protoreflect.ValueOfString(v)
	case int32:
		return protoreflect.ValueOfInt32(v)
	case int:
		return protoreflect.ValueOfInt32(int32(v))
	case bool:
		return protoreflect.ValueOfBool(v)
	case []byte:
		return protoreflect.ValueOfBytes(v)
	case float64:
		return protoreflect.ValueOfFloat64(v)
	}
	t.Fatalf("valueOf: unsupported type %T for field %s", v, fd.Name())
	return protoreflect.Value{}
}

// decodeField pulls one field's value from raw User bytes via
// the same descriptor — exposes what the codec wrote, so
// assertions don't depend on raw byte ordering.
func decodeField(t *testing.T, fdsBytes []byte, raw []byte, fieldName string) (protoreflect.Value, bool) {
	t.Helper()
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(fdsBytes, &fds); err != nil {
		t.Fatalf("decodeField: unmarshal FDS: %v", err)
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		t.Fatalf("decodeField: NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("test.User")
	if err != nil {
		t.Fatalf("decodeField: find User: %v", err)
	}
	desc := d.(protoreflect.MessageDescriptor)
	msg := dynamicpb.NewMessage(desc)
	if err := proto.Unmarshal(raw, msg); err != nil {
		t.Fatalf("decodeField: unmarshal User: %v", err)
	}
	fd := desc.Fields().ByName(protoreflect.Name(fieldName))
	if fd == nil {
		t.Fatalf("decodeField: unknown field %q", fieldName)
	}
	return msg.Get(fd), msg.Has(fd)
}

// TestProtoCodec_NewProtoCodec_RejectsBadDescriptor —
// malformed bytes never resolve.
func TestProtoCodec_NewProtoCodec_RejectsBadDescriptor(t *testing.T) {
	_, err := datamigrate.NewProtoCodec([]byte("garbage"), "test.User")
	if err == nil {
		t.Fatal("expected error for malformed descriptor")
	}
	if !strings.Contains(err.Error(), "decode proto_descriptor") {
		t.Errorf("expected decode error, got: %v", err)
	}
}

// TestProtoCodec_NewProtoCodec_RejectsEmpty — both args
// must be non-empty.
func TestProtoCodec_NewProtoCodec_RejectsEmpty(t *testing.T) {
	if _, err := datamigrate.NewProtoCodec(nil, "test.User"); err == nil {
		t.Error("expected error for empty descriptor bytes")
	}
	fdsBytes, _ := fakeFDS(t)
	if _, err := datamigrate.NewProtoCodec(fdsBytes, ""); err == nil {
		t.Error("expected error for empty messageFQN")
	}
}

// TestProtoCodec_NewProtoCodec_RejectsUnknownMessage — FQN
// not in the descriptor set surfaces as an error.
func TestProtoCodec_NewProtoCodec_RejectsUnknownMessage(t *testing.T) {
	fdsBytes, _ := fakeFDS(t)
	_, err := datamigrate.NewProtoCodec(fdsBytes, "test.NotAMessage")
	if err == nil {
		t.Fatal("expected error for unknown message FQN")
	}
}

// TestProtoCodec_AddFieldDefault_SetsWhenMissing — User
// without `full_name`; ADD_FIELD_DEFAULT writes the default;
// re-running yields no-change.
func TestProtoCodec_AddFieldDefault_SetsWhenMissing(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, err := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	raw := encode(map[string]any{"id": "u1", "email": "x@y.z"})
	out, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "users:*",
		Field: "full_name", Value: "anonymous",
	})
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on missing field")
	}
	if v, has := decodeField(t, fdsBytes, out, "full_name"); !has || v.String() != "anonymous" {
		t.Errorf("full_name = %q (has=%v); want anonymous", v.String(), has)
	}
	// id + email should round-trip unchanged.
	if v, _ := decodeField(t, fdsBytes, out, "id"); v.String() != "u1" {
		t.Errorf("id = %q; want u1", v.String())
	}
	if v, _ := decodeField(t, fdsBytes, out, "email"); v.String() != "x@y.z" {
		t.Errorf("email = %q; want x@y.z", v.String())
	}

	// Re-running on the already-updated value: no change.
	_, changedAgain, err := codec.ApplyOp(out, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "users:*",
		Field: "full_name", Value: "anonymous",
	})
	if err != nil {
		t.Fatalf("ApplyOp re-run: %v", err)
	}
	if changedAgain {
		t.Error("expected changed=false on already-set field (idempotency)")
	}
}

// TestProtoCodec_RemoveField_ClearsWhenPresent — removes a
// set field; re-run is a no-op.
func TestProtoCodec_RemoveField_ClearsWhenPresent(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1", "full_name": "obsolete"})
	out, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRemoveField, Keyspace: "users:*", Field: "full_name",
	})
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on REMOVE_FIELD with present field")
	}
	if _, has := decodeField(t, fdsBytes, out, "full_name"); has {
		t.Errorf("full_name still set after REMOVE")
	}

	_, changedAgain, _ := codec.ApplyOp(out, datamigrate.Operation{
		Op: datamigrate.OpRemoveField, Keyspace: "users:*", Field: "full_name",
	})
	if changedAgain {
		t.Error("REMOVE re-run on absent field should be a no-op")
	}
}

// TestProtoCodec_RenameField_CopiesAndClears — RENAME copies
// the value to To and clears From; re-run is a no-op.
func TestProtoCodec_RenameField_CopiesAndClears(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1", "email": "old@addr"})
	out, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "users:*",
		From: "email", To: "full_name",
	})
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on RENAME with present source")
	}
	if v, has := decodeField(t, fdsBytes, out, "full_name"); !has || v.String() != "old@addr" {
		t.Errorf("full_name = %q (has=%v); want old@addr", v.String(), has)
	}
	if _, has := decodeField(t, fdsBytes, out, "email"); has {
		t.Errorf("email still set after RENAME")
	}
}

// TestProtoCodec_AddFieldDefault_ScalarKinds — every scalar
// kind v2.2 supports must round-trip the typed default. One
// case per kind to keep failures localised.
func TestProtoCodec_AddFieldDefault_ScalarKinds(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")

	cases := []struct {
		field string
		value any
		check func(t *testing.T, v protoreflect.Value)
	}{
		{"age", 18, func(t *testing.T, v protoreflect.Value) {
			if v.Int() != 18 {
				t.Errorf("age = %d; want 18", v.Int())
			}
		}},
		{"active", true, func(t *testing.T, v protoreflect.Value) {
			if !v.Bool() {
				t.Errorf("active = %v; want true", v.Bool())
			}
		}},
		{"score", 9.5, func(t *testing.T, v protoreflect.Value) {
			if v.Float() != 9.5 {
				t.Errorf("score = %v; want 9.5", v.Float())
			}
		}},
		{"ratio", 1.25, func(t *testing.T, v protoreflect.Value) {
			if v.Float() < 1.24 || v.Float() > 1.26 {
				t.Errorf("ratio = %v; want ~1.25", v.Float())
			}
		}},
		{"level64", 1234567890123, func(t *testing.T, v protoreflect.Value) {
			if v.Int() != 1234567890123 {
				t.Errorf("level64 = %d; want 1234567890123", v.Int())
			}
		}},
		{"count32", 42, func(t *testing.T, v protoreflect.Value) {
			if v.Uint() != 42 {
				t.Errorf("count32 = %d; want 42", v.Uint())
			}
		}},
		{"count64", 99, func(t *testing.T, v protoreflect.Value) {
			if v.Uint() != 99 {
				t.Errorf("count64 = %d; want 99", v.Uint())
			}
		}},
		{"payload", "aGVsbG8=", func(t *testing.T, v protoreflect.Value) {
			if string(v.Bytes()) != "hello" {
				t.Errorf("payload = %q; want hello", v.Bytes())
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			raw := encode(map[string]any{"id": "u1"})
			out, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
				Op: datamigrate.OpAddFieldDefault, Keyspace: "users:*",
				Field: c.field, Value: c.value,
			})
			if err != nil {
				t.Fatalf("ApplyOp(%s): %v", c.field, err)
			}
			if !changed {
				t.Fatalf("expected changed=true for %s", c.field)
			}
			v, has := decodeField(t, fdsBytes, out, c.field)
			if !has {
				t.Fatalf("%s not set after ADD_FIELD_DEFAULT", c.field)
			}
			c.check(t, v)
		})
	}
}

// TestProtoCodec_UnknownField_Errors — pointing at a non-
// existent field surfaces an explicit error so the operator
// fixes the migration, not a silent skip.
func TestProtoCodec_UnknownField_Errors(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})
	_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "users:*",
		Field: "no_such_field", Value: "x",
	})
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("expected unknown-field error, got: %v", err)
	}
}

// TestProtoCodec_UnsupportedKinds_Error — v2.2 covers
// scalars only. Repeated / enum / message fields error
// loudly with version-tagged messages so future-graduation
// is obvious.
func TestProtoCodec_UnsupportedKinds_Error(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})

	// Enum
	_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "users:*",
		Field: "status", Value: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "v2.2") {
		t.Errorf("expected v2.2 enum-unsupported error, got %v", err)
	}

	// Repeated string
	_, _, err = codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "users:*",
		Field: "tags", Value: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "v2.2") {
		t.Errorf("expected v2.2 repeated-unsupported error, got %v", err)
	}
}

// TestProtoCodec_EmptyValue_NoOp — zero-byte input
// short-circuits to (nil, false, nil) the same way
// JSONApplyOp does.
func TestProtoCodec_EmptyValue_NoOp(t *testing.T) {
	fdsBytes, _ := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	out, changed, err := codec.ApplyOp(nil, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*", Field: "id", Value: "u1",
	})
	if err != nil {
		t.Errorf("ApplyOp on empty: %v", err)
	}
	if changed || out != nil {
		t.Errorf("expected (nil, false, nil); got out=%v changed=%v", out, changed)
	}
}

// TestProtoCodec_OutOfRangeInt32 — 33-bit value on an int32
// field must error, not silently truncate.
func TestProtoCodec_OutOfRangeInt32(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})
	_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "age", Value: int64(1) << 33,
	})
	if err == nil {
		t.Fatal("expected overflow error")
	}
	if !strings.Contains(err.Error(), "int32 range") {
		t.Errorf("expected int32-range error, got: %v", err)
	}
}

// TestProtoCodec_StringFieldNilValue — nil value on a string
// field is treated as "" (yaml-empty default convention).
func TestProtoCodec_StringFieldNilValue(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})
	out, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "full_name", Value: nil,
	})
	if err != nil || !changed {
		t.Fatalf("nil-on-string should set ''; got changed=%v err=%v", changed, err)
	}
	if v, _ := decodeField(t, fdsBytes, out, "full_name"); v.String() != "" {
		t.Errorf("full_name = %q; want \"\"", v.String())
	}
}

// TestProtoCodec_TypeMismatch — int value on a string field
// surfaces a typed error, not a silent coercion.
func TestProtoCodec_TypeMismatch(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})
	_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "full_name", Value: 42,
	})
	if err == nil || !strings.Contains(err.Error(), "expected string") {
		t.Errorf("got %v, want type-mismatch error", err)
	}

	// Bool→string mismatch.
	_, _, err = codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "active", Value: "yes",
	})
	if err == nil || !strings.Contains(err.Error(), "expected bool") {
		t.Errorf("got %v, want bool type-mismatch", err)
	}
}

// TestProtoCodec_NumericConversions — every yaml-decoded
// numeric width (int / int8…uint64) flows through the
// to{Int,Uint,Float}64 helpers without overflow / type
// errors when the value fits.
func TestProtoCodec_NumericConversions(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})

	cases := []struct {
		name  string
		field string
		val   any
	}{
		// int family → int32 field
		{"int32-from-int8", "age", int8(7)},
		{"int32-from-int16", "age", int16(123)},
		{"int32-from-int32", "age", int32(99)},
		{"int32-from-uint", "age", uint(15)},
		{"int32-from-uint8", "age", uint8(255)},
		{"int32-from-uint16", "age", uint16(65535)},
		// int64 field
		{"int64-from-int", "level64", int(123456)},
		// uint32 field
		{"uint32-from-int", "count32", int(42)},
		{"uint32-from-uint", "count32", uint(42)},
		{"uint32-from-uint64", "count32", uint64(42)},
		// uint64 field
		{"uint64-from-uint64", "count64", uint64(99)},
		// float fields
		{"float-from-int", "ratio", int(2)},
		{"float-from-int64", "ratio", int64(3)},
		{"float-from-float32", "ratio", float32(1.5)},
		{"double-from-int", "score", int(10)},
		{"double-from-int64", "score", int64(20)},
		// nil values for numeric → 0
		{"int32-nil", "age", nil},
		{"int64-nil", "level64", nil},
		{"uint32-nil", "count32", nil},
		{"uint64-nil", "count64", nil},
		{"float-nil", "ratio", nil},
		{"double-nil", "score", nil},
		// bytes nil → nil bytes
		{"bytes-nil", "payload", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
				Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
				Field: c.field, Value: c.val,
			})
			if err != nil {
				t.Errorf("conversion failed: %v", err)
			}
		})
	}
}

// TestProtoCodec_NumericRejection — values that don't fit
// the target field's kind error explicitly.
func TestProtoCodec_NumericRejection(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})

	cases := []struct {
		name  string
		field string
		val   any
		want  string
	}{
		{"uint64-overflows-int64", "level64", uint64(1) << 63, "overflows int64"},
		{"negative-into-uint", "count32", int(-1), "negative"},
		{"negative-int64-into-uint", "count64", int64(-5), "negative"},
		{"uint64-into-uint32-out-of-range", "count32", uint64(1) << 33, "uint32 range"},
		{"string-into-int", "age", "not-a-number", "expected integer"},
		{"string-into-uint", "count32", "not-a-number", "expected unsigned"},
		{"string-into-float", "ratio", "not-a-number", "expected number"},
		{"malformed-base64-bytes", "payload", "not!!base64!!!", "base64 decode"},
		{"int-into-bytes", "payload", 42, "expected string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
				Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
				Field: c.field, Value: c.val,
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want substring %q", err, c.want)
			}
		})
	}
}

// TestProtoCodec_BytesRawSlice — when the value is already a
// []byte (not yaml-coming-from-string), ApplyOp accepts it
// directly without base64-decoding.
func TestProtoCodec_BytesRawSlice(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})
	out, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "payload", Value: []byte("raw"),
	})
	if err != nil || !changed {
		t.Fatalf("ApplyOp: %v changed=%v", err, changed)
	}
	if v, _ := decodeField(t, fdsBytes, out, "payload"); string(v.Bytes()) != "raw" {
		t.Errorf("payload = %q; want \"raw\"", v.Bytes())
	}
}

// TestProtoCodec_RemoveFieldUnknownName — REMOVE_FIELD
// pointing at a non-existent field errors loudly.
func TestProtoCodec_RemoveFieldUnknownName(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})
	_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRemoveField, Keyspace: "x:*", Field: "no_such",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("got %v, want unknown-field error", err)
	}
}

// TestProtoCodec_RenameUnknownFromOrTo — RENAME with bad
// names errors per side.
func TestProtoCodec_RenameUnknownFromOrTo(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})
	_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "x:*",
		From: "no_such", To: "full_name",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("got %v, want unknown-from error", err)
	}
	_, _, err = codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "x:*",
		From: "id", To: "no_such",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("got %v, want unknown-to error", err)
	}
}

// TestProtoCodec_RenameField_RefusesClobber — RENAME into a
// destination that already holds a DIFFERENT value aborts with a
// "refusing to overwrite" error (mirrors JSONApplyOp's clobber
// guard); an identical destination value is benign (source
// cleared, change reported).
func TestProtoCodec_RenameField_RefusesClobber(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")

	// Distinct destination → refuse, leave both fields untouched.
	raw := encode(map[string]any{"email": "old@addr", "full_name": "Existing Name"})
	_, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "users:*",
		From: "email", To: "full_name",
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("got (changed=%v, %v), want refusing-to-overwrite error", changed, err)
	}
	if changed {
		t.Error("expected changed=false on refused clobber")
	}

	// Same destination value → benign: drop source, stay idempotent.
	raw = encode(map[string]any{"email": "dup", "full_name": "dup"})
	out, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "users:*",
		From: "email", To: "full_name",
	})
	if err != nil {
		t.Fatalf("same-value RENAME: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on benign same-value RENAME")
	}
	if v, has := decodeField(t, fdsBytes, out, "full_name"); !has || v.String() != "dup" {
		t.Errorf("full_name = %q (has=%v); want dup", v.String(), has)
	}
	if _, has := decodeField(t, fdsBytes, out, "email"); has {
		t.Error("email still set after benign RENAME")
	}
}

// TestProtoCodec_RenameField_IncompatibleTypes — a RENAME between
// fields of incompatible kind (string→int32) returns a clean
// "incompatible field types" error rather than panicking the
// apply goroutine via dynamicpb's "invalid value type".
func TestProtoCodec_RenameField_IncompatibleTypes(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"email": "old@addr"})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RENAME across incompatible kinds panicked: %v", r)
		}
	}()
	_, changed, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "users:*",
		From: "email", To: "age",
	})
	if err == nil || !strings.Contains(err.Error(), "incompatible field types") {
		t.Errorf("got (changed=%v, %v), want incompatible-field-types error", changed, err)
	}
	if changed {
		t.Error("expected changed=false on incompatible-types RENAME")
	}
}

// TestProtoCodec_UnsupportedOpKind — an op kind the codec
// doesn't recognise (e.g. TRANSFORM_FIELD which has its own
// path) errors at ApplyOp.
func TestProtoCodec_UnsupportedOpKind(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	raw := encode(map[string]any{"id": "u1"})
	_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
		Op: "BREW_COFFEE", Keyspace: "x:*",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported op") {
		t.Errorf("got %v, want unsupported-op error", err)
	}
}

// TestProtoCodec_MalformedProtoBytes — value bytes that
// aren't a valid proto message error at unmarshal.
func TestProtoCodec_MalformedProtoBytes(t *testing.T) {
	fdsBytes, _ := fakeFDS(t)
	codec, _ := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	_, _, err := codec.ApplyOp([]byte{0xFF, 0x00, 0xFF, 0x00}, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*", Field: "id", Value: "u1",
	})
	if err == nil || !strings.Contains(err.Error(), "decode proto") {
		t.Errorf("got %v, want decode-proto error", err)
	}
}
