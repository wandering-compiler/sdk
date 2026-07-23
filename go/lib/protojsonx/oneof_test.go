package protojsonx_test

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/lib/protojsonx"
)

// buildDesc compiles a message M with a genuine oneof contact{Email
// email; string phone} (Email = {string address}) and no aliases — the
// nil-AliasFunc path MCP uses.
func buildDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("px_test.proto"),
		Package: proto.String("pxtest"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Email"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("address"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
			}},
			{Name: proto.String("M"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("email"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".pxtest.Email"), OneofIndex: proto.Int32(0)},
				{Name: proto.String("phone"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), OneofIndex: proto.Int32(0)},
			}, OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("contact")}}},
		},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{file}})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName("pxtest.M"))
	if err != nil {
		t.Fatalf("find M: %v", err)
	}
	if _, err := protoregistry.GlobalFiles.FindFileByPath("px_test.proto"); err != nil {
		_ = protoregistry.GlobalFiles.RegisterFile(d.ParentFile())
	}
	return d.(protoreflect.MessageDescriptor)
}

// Collapse (nil AliasFunc) → discriminated shape; Expand round-trips it
// back through protojson.Unmarshal.
func TestProtojsonx_CollapseExpand_NilAlias(t *testing.T) {
	desc := buildDesc(t)
	if !protojsonx.HasAnyOneof(desc) {
		t.Fatal("M has a genuine oneof")
	}
	m := dynamicpb.NewMessage(desc)
	m.Set(desc.Fields().ByName("id"), protoreflect.ValueOfString("x"))
	emailFd := desc.Fields().ByName("email")
	email := m.NewField(emailFd)
	email.Message().Set(emailFd.Message().Fields().ByName("address"), protoreflect.ValueOfString("a@b.c"))
	m.Set(emailFd, email)

	flat, err := protojson.Marshal(m)
	if err != nil {
		t.Fatalf("protojson: %v", err)
	}
	collapsed, err := protojsonx.CollapseOneofs(flat, desc, nil)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(collapsed, &obj)
	contact, ok := obj["contact"].(map[string]any)
	if !ok || contact["w17_discriminator"] != "email" || contact["address"] != "a@b.c" {
		t.Fatalf("collapsed shape wrong: %s", collapsed)
	}
	if _, leaked := obj["email"]; leaked {
		t.Errorf("flat 'email' leaked: %s", collapsed)
	}

	// Expand back and decode.
	expanded, err := protojsonx.ExpandOneofs(collapsed, desc, nil)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	got := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal(expanded, got); err != nil {
		t.Fatalf("unmarshal expanded: %v", err)
	}
	if !got.Has(emailFd) {
		t.Fatalf("email arm not set after round-trip (expanded=%s)", expanded)
	}
}

// A message with no genuine oneof short-circuits HasAnyOneof.
func TestProtojsonx_NoOneof(t *testing.T) {
	desc := buildDesc(t).Fields().ByName("email").Message() // Email has none
	if protojsonx.HasAnyOneof(desc) {
		t.Error("Email should report no oneof")
	}
}
