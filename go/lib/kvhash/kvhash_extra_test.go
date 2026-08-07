package kvhash

import (
	"context"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestMarshalEntity_NilMessage pins the nil-guard: a nil message
// is a caller bug and must be reported, not panicked on.
func TestMarshalEntity_NilMessage(t *testing.T) {
	if _, err := MarshalEntity(nil); err == nil {
		t.Fatal("MarshalEntity(nil) should error")
	}
}

// TestUnmarshalEntity_NilMessage mirrors the marshal nil-guard on
// the decode side.
func TestUnmarshalEntity_NilMessage(t *testing.T) {
	if err := UnmarshalEntity(map[string]string{"id": "x"}, nil); err == nil {
		t.Fatal("UnmarshalEntity(_, nil) should error")
	}
}

// TestUnmarshalEntity_RefusesNonFlat asserts the flat-schema guard
// runs on the decode path too: a repeated field is rejected before
// any value is read, regardless of which keys the hash carries.
func TestUnmarshalEntity_RefusesNonFlat(t *testing.T) {
	md := msgByName(t, compileFixture(t), "RepeatedEntity")
	dst := dynamicpb.NewMessage(md)
	if err := UnmarshalEntity(map[string]string{"id": "x"}, dst); err == nil {
		t.Fatal("UnmarshalEntity on a repeated field should error")
	}
}

// TestUnmarshalEntity_BoolTrue covers the "true" decode arm — the
// existing round-trip test only exercised the false case.
func TestUnmarshalEntity_BoolTrue(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	dst := dynamicpb.NewMessage(md)
	if err := UnmarshalEntity(map[string]string{"active": "true"}, dst); err != nil {
		t.Fatalf("UnmarshalEntity: %v", err)
	}
	if !dst.ProtoReflect().Get(findField(t, md, "active")).Bool() {
		t.Error("active should decode to true")
	}
}

// TestUnmarshalEntity_NumericParseErrors sweeps every numeric /
// bytes / duration decode arm with a malformed value, asserting
// each returns a typed, field-named error rather than a default
// value.
func TestUnmarshalEntity_NumericParseErrors(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	cases := []struct {
		name  string
		field string
		raw   string
		want  string
	}{
		{name: "int32", field: "score32", raw: "x", want: "invalid int32"},
		{name: "uint32", field: "count32", raw: "-1", want: "invalid uint32"},
		{name: "uint64", field: "count64", raw: "nope", want: "invalid uint64"},
		{name: "float", field: "ratio32", raw: "NaNN", want: "invalid float"},
		{name: "double", field: "ratio64", raw: "1.2.3", want: "invalid double"},
		{name: "bytes", field: "payload", raw: "@@@not-base64@@@", want: "invalid base64 bytes"},
		{name: "duration", field: "ttl", raw: "not-a-duration", want: "invalid duration"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dst := dynamicpb.NewMessage(md)
			err := UnmarshalEntity(map[string]string{tc.field: tc.raw}, dst)
			if err == nil {
				t.Fatalf("expected error decoding %s=%q", tc.field, tc.raw)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestEncodeField_UnknownEnumNumber covers the numeric-fallback
// encode arm: an enum value with no descriptor entry round-trips
// as its decimal number rather than failing.
func TestEncodeField_UnknownEnumNumber(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	statusFd := findField(t, md, "status")
	got, err := encodeField(statusFd, protoreflect.ValueOfEnum(99))
	if err != nil {
		t.Fatalf("encodeField: %v", err)
	}
	if got != "99" {
		t.Errorf("unknown enum encode = %q, want \"99\"", got)
	}
}

// TestEncodeField_UnsupportedWKT covers the well-known-type
// fall-through: a nested non-WKT message reaching encodeField
// directly (bypassing the upstream checkFlatField filter) errors
// rather than emitting garbage.
func TestEncodeField_UnsupportedWKT(t *testing.T) {
	md := msgByName(t, compileFixture(t), "NestedEntity")
	innerFd := findField(t, md, "inner")
	innerMsg := dynamicpb.NewMessage(innerFd.Message())
	_, err := encodeField(innerFd, protoreflect.ValueOfMessage(innerMsg.ProtoReflect()))
	if err == nil {
		t.Fatal("encodeField on a non-WKT nested message should error")
	}
	if !contains(err.Error(), "unsupported well-known type") {
		t.Errorf("unexpected error: %v", err)
	}
}

// compileGroupFixture compiles the proto2 group fixture used to
// drive the GroupKind defensive branches.
func compileGroupFixture(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: []string{"testdata"},
		}),
	}
	files, err := c.Compile(context.Background(), "kvhash_group_fixture.proto")
	if err != nil {
		t.Fatalf("compile group fixture: %v", err)
	}
	f := files.FindFileByPath("kvhash_group_fixture.proto")
	if f == nil {
		t.Fatal("kvhash_group_fixture.proto not found in compile output")
	}
	return f
}

// groupField returns the single GroupKind field of GroupEntity.
func groupField(t *testing.T) protoreflect.FieldDescriptor {
	t.Helper()
	md := msgByName(t, compileGroupFixture(t), "GroupEntity")
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if fd.Kind() == protoreflect.GroupKind {
			return fd
		}
	}
	t.Fatal("no GroupKind field found on GroupEntity")
	return nil
}

// TestCheckFlatField_RejectsGroup covers the proto2-group rejection
// arm of checkFlatField — proto3 input can't reach it, so the
// fixture supplies a real group descriptor.
func TestCheckFlatField_RejectsGroup(t *testing.T) {
	err := checkFlatField(groupField(t))
	if err == nil {
		t.Fatal("checkFlatField should reject a proto2 group")
	}
	if !contains(err.Error(), "group") {
		t.Errorf("error should mention group: %v", err)
	}
}

// TestEncodeField_RejectsGroup covers encodeField's unsupported-
// kind fall-through using the group descriptor.
func TestEncodeField_RejectsGroup(t *testing.T) {
	_, err := encodeField(groupField(t), protoreflect.Value{})
	if err == nil {
		t.Fatal("encodeField should reject an unsupported kind")
	}
	if !contains(err.Error(), "unsupported kind") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDecodeField_RejectsGroup covers decodeField's unsupported-
// kind fall-through using the group descriptor.
func TestDecodeField_RejectsGroup(t *testing.T) {
	_, err := decodeField(groupField(t), "anything")
	if err == nil {
		t.Fatal("decodeField should reject an unsupported kind")
	}
	if !contains(err.Error(), "unsupported kind") {
		t.Errorf("unexpected error: %v", err)
	}
}
