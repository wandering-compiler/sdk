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

// richFDS hand-builds a FileDescriptorSet whose `test.User` carries the
// composite field shapes fakeFDS lacks: a singular message (`profile`),
// a map (`attrs`), and a repeated scalar (`tags`). These drive the
// message / map / repeated arms of fieldShape and scalarValue.
func richFDS(t *testing.T) []byte {
	t.Helper()
	stringT := descriptorpb.FieldDescriptorProto_TYPE_STRING
	messageT := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	syntax := "proto3"
	pkg := "test"
	mapEntry := true

	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("rich.proto"),
		Package: &pkg,
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Profile"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("bio"), Number: proto.Int32(1), Type: &stringT, Label: &optional},
				},
			},
			{
				Name: proto.String("User"),
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:    proto.String("AttrsEntry"),
					Options: &descriptorpb.MessageOptions{MapEntry: &mapEntry},
					Field: []*descriptorpb.FieldDescriptorProto{
						{Name: proto.String("key"), Number: proto.Int32(1), Type: &stringT, Label: &optional},
						{Name: proto.String("value"), Number: proto.Int32(2), Type: &stringT, Label: &optional},
					},
				}},
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("id"), Number: proto.Int32(1), Type: &stringT, Label: &optional},
					{Name: proto.String("email"), Number: proto.Int32(2), Type: &stringT, Label: &optional},
					{Name: proto.String("tags"), Number: proto.Int32(3), Type: &stringT, Label: &repeated},
					{Name: proto.String("profile"), Number: proto.Int32(4), Type: &messageT, TypeName: proto.String(".test.Profile"), Label: &optional},
					{Name: proto.String("attrs"), Number: proto.Int32(5), Type: &messageT, TypeName: proto.String(".test.User.AttrsEntry"), Label: &repeated},
				},
			},
		},
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	b, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal rich FDS: %v", err)
	}
	return b
}

// richUserDesc resolves the test.User descriptor from a rich FDS.
func richUserDesc(t *testing.T, fdsBytes []byte) protoreflect.MessageDescriptor {
	t.Helper()
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(fdsBytes, &fds); err != nil {
		t.Fatalf("unmarshal rich FDS: %v", err)
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("test.User")
	if err != nil {
		t.Fatalf("find User: %v", err)
	}
	return d.(protoreflect.MessageDescriptor)
}

// richUserBytes encodes a User with the composite fields populated so a
// RENAME's `msg.Has(fromFD)` guard fires and reaches the shape check.
func richUserBytes(t *testing.T, fdsBytes []byte) []byte {
	t.Helper()
	desc := richUserDesc(t, fdsBytes)
	msg := dynamicpb.NewMessage(desc)
	msg.Set(desc.Fields().ByName("id"), protoreflect.ValueOfString("u1"))

	tagsFD := desc.Fields().ByName("tags")
	tags := msg.NewField(tagsFD)
	tags.List().Append(protoreflect.ValueOfString("a"))
	msg.Set(tagsFD, tags)

	profFD := desc.Fields().ByName("profile")
	prof := dynamicpb.NewMessage(profFD.Message())
	prof.Set(profFD.Message().Fields().ByName("bio"), protoreflect.ValueOfString("hi"))
	msg.Set(profFD, protoreflect.ValueOfMessage(prof))

	attrsFD := desc.Fields().ByName("attrs")
	attrs := msg.NewField(attrsFD)
	attrs.Map().Set(protoreflect.ValueOfString("k").MapKey(), protoreflect.ValueOfString("v"))
	msg.Set(attrsFD, attrs)

	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal rich User: %v", err)
	}
	return raw
}

// TestProtoCodec_RenameIncompatibleShapes pins the RENAME shape guard's
// fieldShape rendering across composite kinds: a singular message prints
// its FQN, a map prints `map <entry-FQN>`, and a repeated scalar prints
// `repeated <kind>`. Each rename targets the scalar `email`, so the
// kind/cardinality mismatch is refused with both shapes named.
func TestProtoCodec_RenameIncompatibleShapes(t *testing.T) {
	fdsBytes := richFDS(t)
	raw := richUserBytes(t, fdsBytes)
	codec, err := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	cases := []struct {
		name, from, frag string
	}{
		{"message field", "profile", "test.Profile"},
		{"map field", "attrs", "map test.User.AttrsEntry"},
		{"repeated field", "tags", "repeated string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
				Op: datamigrate.OpRenameField, Keyspace: "users:*", From: c.from, To: "email",
			})
			if err == nil || !strings.Contains(err.Error(), "incompatible") || !strings.Contains(err.Error(), c.frag) {
				t.Fatalf("rename %s→email err = %v, want incompatible naming %q", c.from, err, c.frag)
			}
		})
	}
}

// TestProtoCodec_ScalarValueUnsupportedKinds pins scalarValue's refusal
// of composite field kinds via ADD_FIELD_DEFAULT on an absent field: a
// message field and a map field both error with an explicit
// "unsupported in v2.2" tag rather than panicking dynamicpb.Set.
func TestProtoCodec_ScalarValueUnsupportedKinds(t *testing.T) {
	fdsBytes := richFDS(t)
	codec, err := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	// Minimal User: only id — profile/attrs absent so ADD_FIELD reaches scalarValue.
	desc := richUserDesc(t, fdsBytes)
	msg := dynamicpb.NewMessage(desc)
	msg.Set(desc.Fields().ByName("id"), protoreflect.ValueOfString("u1"))
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal minimal User: %v", err)
	}

	cases := []struct{ name, field, frag string }{
		{"message kind", "profile", "message / group field kind unsupported"},
		{"map kind", "attrs", "map field kind unsupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := codec.ApplyOp(raw, datamigrate.Operation{
				Op: datamigrate.OpAddFieldDefault, Keyspace: "users:*", Field: c.field, Value: "x",
			})
			if err == nil || !strings.Contains(err.Error(), c.frag) {
				t.Fatalf("ADD_FIELD %s err = %v, want fragment %q", c.field, err, c.frag)
			}
		})
	}
}

// TestProtoCodec_AddDoubleNonNumeric pins scalarValue's DoubleKind error
// arm: a non-numeric YAML value on a double field surfaces a kind-tagged
// conversion error instead of a silent zero.
func TestProtoCodec_AddDoubleNonNumeric(t *testing.T) {
	fdsBytes, encode := fakeFDS(t)
	codec, err := datamigrate.NewProtoCodec(fdsBytes, "test.User")
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	raw := encode(map[string]any{"id": "u1"})
	_, _, err = codec.ApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "users:*", Field: "score", Value: "not-a-number",
	})
	if err == nil || !strings.Contains(err.Error(), "kind=double") {
		t.Fatalf("ADD_FIELD score=string err = %v, want double-kind conversion error", err)
	}
}

// TestProtoCodec_NewProtoCodec_NonMessageFQN pins the "FQN resolves but
// isn't a message" arm: pointing proto_message at an enum (test.Status)
// is refused with a "want message" diagnostic.
func TestProtoCodec_NewProtoCodec_NonMessageFQN(t *testing.T) {
	fdsBytes, _ := fakeFDS(t)
	_, err := datamigrate.NewProtoCodec(fdsBytes, "test.Status")
	if err == nil || !strings.Contains(err.Error(), "want message") {
		t.Fatalf("NewProtoCodec(enum FQN) err = %v, want non-message refusal", err)
	}
}

// TestProtoCodec_NewProtoCodec_RegistryBuildError pins the descriptor-
// registry build failure arm: an FDS that decodes but references an
// undefined message type fails protodesc.NewFiles with operator context.
func TestProtoCodec_NewProtoCodec_RegistryBuildError(t *testing.T) {
	stringT := descriptorpb.FieldDescriptorProto_TYPE_STRING
	messageT := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	syntax := "proto3"
	pkg := "test"
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("bad.proto"),
		Package: &pkg,
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("User"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Type: &stringT, Label: &optional},
				// References a message that does not exist in the set →
				// protodesc.NewFiles can't resolve it.
				{Name: proto.String("ghost"), Number: proto.Int32(2), Type: &messageT, TypeName: proto.String(".test.DoesNotExist"), Label: &optional},
			},
		}},
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	fdsBytes, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal bad FDS: %v", err)
	}
	_, err = datamigrate.NewProtoCodec(fdsBytes, "test.User")
	if err == nil || !strings.Contains(err.Error(), "build descriptor registry") {
		t.Fatalf("NewProtoCodec(unresolvable FDS) err = %v, want registry-build error", err)
	}
}
