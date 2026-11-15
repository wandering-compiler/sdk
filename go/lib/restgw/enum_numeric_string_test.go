package restgw_test

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// enumReqDesc: message Req { Status status = 1; repeated Status many = 2;
// Inner inner = 3; map<string, Status> by = 4; string note = 5; int64 id = 6; }
// message Inner { Status status = 1; }
func enumReqDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	enumT := descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum()
	msgT := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	strT := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	file := &descriptorpb.FileDescriptorProto{
		Name: proto.String("rge_test.proto"), Package: proto.String("rge"), Syntax: proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Status"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("STATUS_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("STATUS_OPEN"), Number: proto.Int32(1)},
				{Name: proto.String("STATUS_DONE"), Number: proto.Int32(3)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Inner"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("status"), Number: proto.Int32(1), Label: opt, Type: enumT, TypeName: proto.String(".rge.Status")},
			}},
			{Name: proto.String("Req"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("status"), Number: proto.Int32(1), Label: opt, Type: enumT, TypeName: proto.String(".rge.Status")},
				{Name: proto.String("many"), Number: proto.Int32(2), Label: rep, Type: enumT, TypeName: proto.String(".rge.Status")},
				{Name: proto.String("inner"), Number: proto.Int32(3), Label: opt, Type: msgT, TypeName: proto.String(".rge.Inner")},
				{Name: proto.String("by"), Number: proto.Int32(4), Label: rep, Type: msgT, TypeName: proto.String(".rge.Req.ByEntry")},
				{Name: proto.String("note"), Number: proto.Int32(5), Label: opt, Type: strT},
				{Name: proto.String("id"), Number: proto.Int32(6), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
			}, NestedType: []*descriptorpb.DescriptorProto{{
				Name:    proto.String("ByEntry"),
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("key"), Number: proto.Int32(1), Label: opt, Type: strT},
					{Name: proto.String("value"), Number: proto.Int32(2), Label: opt, Type: enumT, TypeName: proto.String(".rge.Status")},
				},
			}}},
		},
	}
	fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	return fd.Messages().ByName("Req")
}

// TestUnmarshalProto_EnumNumericString: the w17 wire carries an enum as its
// NUMBER, and every w17 client also produces the number as a STRING —
// the generated TS/JS clients keep a value their build does not name as
// its numeric string ("7"), and the rpc-cli quotes every enum flag. Stock
// protojson reads a quoted enum as a value NAME and, with DiscardUnknown,
// silently drops one it does not know — so a read-modify-write reset the
// value and a CLI filter became no filter, with no error either way.
func TestUnmarshalProto_EnumNumericString(t *testing.T) {
	desc := enumReqDesc(t)
	msg := dynamicpb.NewMessage(desc)
	in := `{"status":"3","many":["1","STATUS_DONE",3,"7"],"inner":{"status":"7"},"by":{"k":"3"},"note":"3","id":"3"}`
	if err := restgw.UnmarshalProto([]byte(in), msg); err != nil {
		t.Fatalf("UnmarshalProto: %v", err)
	}
	f := desc.Fields()
	if got := msg.Get(f.ByName("status")).Enum(); got != 3 {
		t.Errorf(`"status":"3" decoded to %d, want 3`, got)
	}
	many := msg.Get(f.ByName("many")).List()
	var gotMany []protoreflect.EnumNumber
	for i := 0; i < many.Len(); i++ {
		gotMany = append(gotMany, many.Get(i).Enum())
	}
	if len(gotMany) != 4 || gotMany[0] != 1 || gotMany[1] != 3 || gotMany[2] != 3 || gotMany[3] != 7 {
		t.Errorf(`"many" decoded to %v, want [1 3 3 7]`, gotMany)
	}
	inner := msg.Get(f.ByName("inner")).Message()
	if got := inner.Get(inner.Descriptor().Fields().ByName("status")).Enum(); got != 7 {
		t.Errorf(`"inner.status":"7" (a value this server build does not name) decoded to %d, want 7`, got)
	}
	by := msg.Get(f.ByName("by")).Map()
	if got := by.Get(protoreflect.ValueOfString("k").MapKey()).Enum(); got != 3 {
		t.Errorf(`"by.k":"3" decoded to %d, want 3`, got)
	}
	// Strings of other kinds are not touched.
	if got := msg.Get(f.ByName("note")).String(); got != "3" {
		t.Errorf(`"note" = %q, want "3"`, got)
	}
	if got := msg.Get(f.ByName("id")).Int(); got != 3 {
		t.Errorf(`"id" = %d, want 3`, got)
	}
}
