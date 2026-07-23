package restgw

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// oneofTestDesc builds a message M with:
//   - id            (plain string, field 1)
//   - oneof contact { Email email = 2; string phone = 3; }  (GENUINE)
//   - optional nick (field 4, proto3 optional → SYNTHETIC oneof)
//
// Email is a nested message { string address = 1; }.
func oneofTestDesc(t testing.TB) protoreflect.MessageDescriptor {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("oneof_test.proto"),
		Package:    proto.String("oneoftest"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"w17/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Email"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name: proto.String("address"), Number: proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("address"),
					},
				},
			},
			{
				Name: proto.String("M"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name: proto.String("id"), Number: proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("id"),
					},
					{
						Name: proto.String("email"), Number: proto.Int32(2),
						Label:      descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:       descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName:   proto.String(".oneoftest.Email"),
						JsonName:   proto.String("email"),
						OneofIndex: proto.Int32(0),
					},
					{
						Name: proto.String("phone"), Number: proto.Int32(3),
						Label:      descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:       descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName:   proto.String("phone"),
						OneofIndex: proto.Int32(0),
					},
					{
						Name: proto.String("nick"), Number: proto.Int32(4),
						Label:          descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:           descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName:       proto.String("nick"),
						OneofIndex:     proto.Int32(1),
						Proto3Optional: proto.Bool(true),
					},
				},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{
					{Name: proto.String("contact")},
					{Name: proto.String("_nick")},
				},
			},
		},
	}
	return buildMessageDescriptor(t, file, "oneoftest.M")
}

// A message with NO genuine oneof takes the unchanged protojson path —
// MarshalProto output must be byte-identical to a direct protojson
// marshal (the collapse pass is gated off). Email (a oneofTestDesc
// sub-message) has no oneof.
func TestOneof_NoOneof_ByteIdenticalToProtojson(t *testing.T) {
	desc := oneofTestDesc(t).Fields().ByName("email").Message()
	m := dynamicpb.NewMessage(desc)
	m.Set(desc.Fields().ByName("address"), protoreflect.ValueOfString("a@b.c"))

	got, err := MarshalProto(m)
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}
	want, err := Marshaller.Marshal(m)
	if err != nil {
		t.Fatalf("protojson: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("oneof-free message must be byte-identical to protojson;\n got=%s\nwant=%s", got, want)
	}
	if hasAnyOneof(desc) {
		t.Error("Email should report hasAnyOneof=false")
	}
}

func TestOneof_HasAnyOneof_GenuineVsSynthetic(t *testing.T) {
	desc := oneofTestDesc(t)
	if !hasAnyOneof(desc) {
		t.Fatal("M has a genuine oneof (contact); want hasAnyOneof=true")
	}
	// Email has no oneof at all.
	email := desc.Fields().ByName("email").Message()
	if hasAnyOneof(email) {
		t.Error("Email has no oneof; want hasAnyOneof=false")
	}
}

// message arm collapses to {oneofName: {..., w17_discriminator}} and
// round-trips back to the same arm.
func TestOneof_MessageArm_Collapse_RoundTrip(t *testing.T) {
	desc := oneofTestDesc(t)
	m := dynamicpb.NewMessage(desc)
	m.Set(desc.Fields().ByName("id"), protoreflect.ValueOfString("x-1"))
	emailFd := desc.Fields().ByName("email")
	email := m.NewField(emailFd)
	email.Message().Set(emailFd.Message().Fields().ByName("address"), protoreflect.ValueOfString("a@b.c"))
	m.Set(emailFd, email)

	raw, err := MarshalProto(m)
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("json: %v (raw=%s)", err, raw)
	}
	if obj["id"] != "x-1" {
		t.Errorf("id = %v, want x-1", obj["id"])
	}
	if _, ok := obj["email"]; ok {
		t.Errorf("flat 'email' key should be gone; got %s", raw)
	}
	contact, ok := obj["contact"].(map[string]any)
	if !ok {
		t.Fatalf("'contact' should be the collapsed object; got %s", raw)
	}
	if contact[DiscriminatorKey] != "email" {
		t.Errorf("discriminator = %v, want email", contact[DiscriminatorKey])
	}
	if contact["address"] != "a@b.c" {
		t.Errorf("address = %v, want a@b.c", contact["address"])
	}

	// Round-trip back.
	got := dynamicpb.NewMessage(desc)
	if err := UnmarshalProto(raw, got); err != nil {
		t.Fatalf("UnmarshalProto: %v", err)
	}
	if !got.Has(emailFd) {
		t.Fatalf("email arm not set after round-trip")
	}
	addr := got.Get(emailFd).Message().Get(emailFd.Message().Fields().ByName("address")).String()
	if addr != "a@b.c" {
		t.Errorf("round-trip address = %q, want a@b.c", addr)
	}
	if got.Get(desc.Fields().ByName("id")).String() != "x-1" {
		t.Errorf("round-trip id mismatch")
	}
}

// scalar arm collapses to a bare value (no discriminator) and is
// matched back by JSON type on decode.
func TestOneof_ScalarArm_BareValue_RoundTrip(t *testing.T) {
	desc := oneofTestDesc(t)
	m := dynamicpb.NewMessage(desc)
	phoneFd := desc.Fields().ByName("phone")
	m.Set(phoneFd, protoreflect.ValueOfString("+420555"))

	raw, err := MarshalProto(m)
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["contact"] != "+420555" {
		t.Errorf("scalar arm should collapse to bare value; got %v (raw=%s)", obj["contact"], raw)
	}

	got := dynamicpb.NewMessage(desc)
	if err := UnmarshalProto(raw, got); err != nil {
		t.Fatalf("UnmarshalProto: %v", err)
	}
	if !got.Has(phoneFd) {
		t.Fatalf("phone arm not set after round-trip (raw=%s)", raw)
	}
	if got.Get(phoneFd).String() != "+420555" {
		t.Errorf("round-trip phone = %q", got.Get(phoneFd).String())
	}
}

// a proto3 `optional` field (synthetic oneof) is left flat — NOT
// collapsed — even though the message carries a genuine oneof too.
func TestOneof_Proto3Optional_NotCollapsed(t *testing.T) {
	desc := oneofTestDesc(t)
	m := dynamicpb.NewMessage(desc)
	m.Set(desc.Fields().ByName("nick"), protoreflect.ValueOfString("nicky"))

	raw, err := MarshalProto(m)
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["nick"] != "nicky" {
		t.Errorf("proto3-optional 'nick' should stay flat; got %s", raw)
	}
	if _, ok := obj["_nick"]; ok {
		t.Errorf("synthetic oneof '_nick' must never appear; got %s", raw)
	}
}
