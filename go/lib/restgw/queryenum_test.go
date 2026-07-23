package restgw

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// B3 — enum request fields had no URL form at all: the decode generator,
// the OpenAPI emitter and the client emitter all skipped them, so on a
// body-less method (GET / SSE stream) the field was silently dropped and
// no body existed to carry it instead. SetEnumField gives them one.

// enumFieldDescriptor builds `message Q { Status status = 1; }` with
// `enum Status { STATUS_UNSPECIFIED = 0; OPEN = 1; DONE = 3; }`.
func enumFieldDescriptor(t testing.TB) protoreflect.MessageDescriptor {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("queryenum_test.proto"),
		Package: proto.String("restgwtest"),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Status"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("STATUS_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("OPEN"), Number: proto.Int32(1)},
				{Name: proto.String("DONE"), Number: proto.Int32(3)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Q"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:     proto.String("status"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
					TypeName: proto.String(".restgwtest.Status"),
					JsonName: proto.String("status"),
				},
				{
					Name:     proto.String("name"),
					Number:   proto.Int32(2),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					JsonName: proto.String("name"),
				},
			},
		}},
	}
	return buildMessageDescriptor(t, file, "restgwtest.Q")
}

func statusOf(t testing.TB, msg proto.Message) protoreflect.EnumNumber {
	t.Helper()
	m := msg.ProtoReflect()
	return m.Get(m.Descriptor().Fields().ByName("status")).Enum()
}

// The protojson spelling (the value NAME) is the primary form — a query
// param and a JSON body then name the value identically.
func TestSetEnumField_ByName(t *testing.T) {
	msg := dynamicpb.NewMessage(enumFieldDescriptor(t))
	if err := SetEnumField(msg, "status", "DONE"); err != nil {
		t.Fatalf("SetEnumField: %v", err)
	}
	if got := statusOf(t, msg); got != 3 {
		t.Errorf("status = %d, want 3 (DONE)", got)
	}
}

// The decimal form is accepted too, matching protojson.
func TestSetEnumField_ByNumber(t *testing.T) {
	msg := dynamicpb.NewMessage(enumFieldDescriptor(t))
	if err := SetEnumField(msg, "status", "1"); err != nil {
		t.Fatalf("SetEnumField: %v", err)
	}
	if got := statusOf(t, msg); got != 1 {
		t.Errorf("status = %d, want 1 (OPEN)", got)
	}
}

// An undeclared value is refused, not silently accepted. proto3 tolerates
// unknown enum numbers on the WIRE for forward compatibility, but a URL a
// caller typed is a typo, and passing it through would hand the backend a
// value with no meaning.
func TestSetEnumField_RejectsUnknown(t *testing.T) {
	for _, raw := range []string{"NOPE", "7", "done"} {
		msg := dynamicpb.NewMessage(enumFieldDescriptor(t))
		err := SetEnumField(msg, "status", raw)
		if err == nil {
			t.Errorf("SetEnumField(%q) = nil error, want refusal", raw)
			continue
		}
		// The diagnostic has to be actionable — name the field and list
		// what would have worked.
		if !strings.Contains(err.Error(), "status") || !strings.Contains(err.Error(), "OPEN") {
			t.Errorf("SetEnumField(%q) error should name the field + accepted values; got %v", raw, err)
		}
	}
}

// Misuse is reported rather than panicking: unknown field, wrong kind,
// nil message.
func TestSetEnumField_Misuse(t *testing.T) {
	msg := dynamicpb.NewMessage(enumFieldDescriptor(t))
	if err := SetEnumField(msg, "missing", "OPEN"); err == nil {
		t.Error("unknown field should error")
	}
	if err := SetEnumField(msg, "name", "OPEN"); err == nil {
		t.Error("non-enum field should error")
	}
	if err := SetEnumField(nil, "status", "OPEN"); err == nil {
		t.Error("nil message should error")
	}
}
