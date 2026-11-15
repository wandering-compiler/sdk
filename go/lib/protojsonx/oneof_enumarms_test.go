package protojsonx_test

import (
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

// enumArmDesc compiles
//
//	enum Color { COLOR_UNSPECIFIED = 0; RED = 1; GREEN = 2; }
//	message M {
//	  oneof pick { string s = 1; Color c = 2; }
//	  oneof sel  { Color c2 = 3; bool b = 4; }
//	}
//
// The generated TS/JS client's surface for an enum is the value NAME, and in
// JSON mode it sends the request body as written — so a bare enum arm reaches
// the gateway as a JSON string. Plain enum fields accept a name through
// protojson; a bare oneof arm must too, instead of landing in the string arm
// (pick) or being dropped (sel).
func enumArmDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	arm := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, idx int32, typeName string) *descriptorpb.FieldDescriptorProto {
		fd := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Label: opt, Type: typ.Enum(), OneofIndex: proto.Int32(idx)}
		if typeName != "" {
			fd.TypeName = proto.String(typeName)
		}
		return fd
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("pxe_test.proto"),
		Package: proto.String("pxe"),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Color"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("COLOR_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("RED"), Number: proto.Int32(1)},
				{Name: proto.String("GREEN"), Number: proto.Int32(2)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("M"),
			Field: []*descriptorpb.FieldDescriptorProto{
				arm("s", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, 0, ""),
				arm("c", 2, descriptorpb.FieldDescriptorProto_TYPE_ENUM, 0, ".pxe.Color"),
				arm("c2", 3, descriptorpb.FieldDescriptorProto_TYPE_ENUM, 1, ".pxe.Color"),
				arm("b", 4, descriptorpb.FieldDescriptorProto_TYPE_BOOL, 1, ""),
			},
			OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("pick")}, {Name: proto.String("sel")}},
		}},
	}
	fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	return fd.Messages().ByName("M")
}

// decodeExpanded runs the gateway's request path — ExpandOneofs, then
// protojson with DiscardUnknown — and returns the decoded message.
func decodeExpanded(t *testing.T, desc protoreflect.MessageDescriptor, in string) *dynamicpb.Message {
	t.Helper()
	out, err := protojsonx.ExpandOneofs([]byte(in), desc, nil)
	if err != nil {
		t.Fatalf("expand %s: %v", in, err)
	}
	msg := dynamicpb.NewMessage(desc)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(out, msg); err != nil {
		t.Fatalf("unmarshal %s (expanded %s): %v", in, out, err)
	}
	return msg
}

func TestExpand_BareEnumArmByName(t *testing.T) {
	desc := enumArmDesc(t)
	f := desc.Fields()
	cases := []struct {
		in    string
		field string
		want  any // enum number or string value
	}{
		{`{"pick":"GREEN"}`, "c", protoreflect.EnumNumber(2)},             // name → enum arm, not the string arm
		{`{"pick":"COLOR_UNSPECIFIED"}`, "c", protoreflect.EnumNumber(0)}, // the zero name is a member too
		{`{"pick":"hello"}`, "s", "hello"},                                // a non-member string stays the string arm
		{`{"pick":2}`, "c", protoreflect.EnumNumber(2)},                   // the number (the wire form) still works
		{`{"sel":"RED"}`, "c2", protoreflect.EnumNumber(1)},               // no string arm: name used to be dropped
		{`{"sel":true}`, "b", true},
	}
	for _, tc := range cases {
		msg := decodeExpanded(t, desc, tc.in)
		fd := f.ByName(protoreflect.Name(tc.field))
		if !msg.Has(fd) {
			t.Errorf("%s: arm %q not set; decoded %v", tc.in, tc.field, msg)
			continue
		}
		var got any
		switch fd.Kind() {
		case protoreflect.EnumKind:
			got = msg.Get(fd).Enum()
		case protoreflect.StringKind:
			got = msg.Get(fd).String()
		case protoreflect.BoolKind:
			got = msg.Get(fd).Bool()
		}
		if got != tc.want {
			t.Errorf("%s: arm %q = %v, want %v", tc.in, tc.field, got, tc.want)
		}
	}
}
