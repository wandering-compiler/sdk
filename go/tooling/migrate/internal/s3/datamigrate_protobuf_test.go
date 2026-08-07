package s3_test

import (
	"encoding/base64"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// userFDS hand-builds a FileDescriptorSet for a synthetic
// `test.User { string id, string full_name }` message and returns
// (base64 of the FDS bytes, the resolved descriptor). The base64 is what
// a migration's `proto_descriptor` YAML field carries.
func userFDS(t *testing.T) (string, protoreflect.MessageDescriptor) {
	t.Helper()
	stringT := descriptorpb.FieldDescriptorProto_TYPE_STRING
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	syntax := "proto3"
	pkg := "test"
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("user.proto"),
		Package: &pkg,
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("User"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Type: &stringT, Label: &optional},
				{Name: proto.String("full_name"), Number: proto.Int32(2), Type: &stringT, Label: &optional},
			},
		}},
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	raw, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal FDS: %v", err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("test.User")
	if err != nil {
		t.Fatalf("find User: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw), d.(protoreflect.MessageDescriptor)
}

// TestApply_YAMLProtobufCodec_S3 pins the protobuf-encoded data migration
// through the ProtoCodec apply branch in applyOpToObject (the per-object
// codec path the JSON tests never reach): a proto-encoded object is GET,
// the codec sets the missing scalar default, and the mutated proto is PUT
// back — observable by decoding the stored bytes.
func TestApply_YAMLProtobufCodec_S3(t *testing.T) {
	f := newFakeS3()
	b64, desc := userFDS(t)

	// Store a User with only id set (full_name absent).
	msg := dynamicpb.NewMessage(desc)
	msg.Set(desc.Fields().ByName("id"), protoreflect.ValueOfString("u1"))
	rawBytes, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal User: %v", err)
	}
	f.objects["users/1"] = rawBytes

	body := fmt.Sprintf(`version: 1
encoding: protobuf
proto_descriptor: %s
proto_message: test.User
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: users/
    field: full_name
    value: anonymous
`, b64)
	a := newApplier(t, f)
	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "20260601T000000Z", UpSql: body}); err != nil {
		t.Fatalf("Apply protobuf YAML: %v", err)
	}

	out := dynamicpb.NewMessage(desc)
	if err := proto.Unmarshal(f.objects["users/1"], out); err != nil {
		t.Fatalf("decode migrated User: %v", err)
	}
	if fn := out.Get(desc.Fields().ByName("full_name")).String(); fn != "anonymous" {
		t.Errorf("full_name = %q, want anonymous (codec apply branch)", fn)
	}
}
