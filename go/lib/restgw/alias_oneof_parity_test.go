package restgw

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
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// aliasOneofFile is a schema with rest_alias fields at every depth below a
// oneof: on an arm message's own fields, on a message nested in an arm, on a
// field of a oneof nested in an arm, on the arm field itself, beside a scalar
// arm and an object-WKT arm, in a list and a map of oneof-carrying messages,
// (An arm field reaching the wire as w17_discriminator is refused at build by
// the gateway's schema guard, so it has no wire to compare.)
func aliasOneofFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	msgT := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	alias := func(a string) *descriptorpb.FieldOptions {
		o := &descriptorpb.FieldOptions{}
		proto.SetExtension(o, w17pb.E_Field, &w17pb.Field{RestAlias: a})
		return o
	}
	oi := func(i int32) *int32 { return &i }
	f := func(name string, num int32, typ *descriptorpb.FieldDescriptorProto_Type, typeName string, o *descriptorpb.FieldOptions) *descriptorpb.FieldDescriptorProto {
		fd := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Type: typ, Label: opt, Options: o}
		if typeName != "" {
			fd.TypeName = proto.String(typeName)
		}
		return fd
	}
	inOneof := func(fd *descriptorpb.FieldDescriptorProto, i int32) *descriptorpb.FieldDescriptorProto {
		fd.OneofIndex = oi(i)
		return fd
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("aliasparity/x.proto"),
		Package:    proto.String("aliasparity"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"w17/field.proto", "google/protobuf/struct.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Leaf"), Field: []*descriptorpb.FieldDescriptorProto{
				f("leaf_note", 1, str, "", alias("leafNote")),
			}},
			{Name: proto.String("Inner"), Field: []*descriptorpb.FieldDescriptorProto{
				inOneof(f("deep", 1, msgT, ".aliasparity.Leaf", nil), 0),
				inOneof(f("deep_text", 2, str, "", nil), 0),
			}, OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("pick")}}},
			{Name: proto.String("Arm"), Field: []*descriptorpb.FieldDescriptorProto{
				f("arm_label", 1, str, "", alias("armLabel")),
				f("leaf", 2, msgT, ".aliasparity.Leaf", alias("leafy")),
				f("inner", 3, msgT, ".aliasparity.Inner", nil),
				f("plain", 5, str, "", nil),
			}},
			{Name: proto.String("Holder"), Field: []*descriptorpb.FieldDescriptorProto{
				f("top", 1, str, "", alias("topAlias")),
				inOneof(f("arm", 2, msgT, ".aliasparity.Arm", nil), 0),
				inOneof(f("aliased_arm", 3, msgT, ".aliasparity.Arm", alias("armX")), 0),
				inOneof(f("bare", 4, str, "", alias("bareX")), 0),
				inOneof(f("st", 5, msgT, ".google.protobuf.Struct", nil), 0),
			}, OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("sel")}}},
			{Name: proto.String("Root"), Field: []*descriptorpb.FieldDescriptorProto{
				f("one", 1, msgT, ".aliasparity.Holder", nil),
				{Name: proto.String("many"), Number: proto.Int32(2), Type: msgT, Label: rep, TypeName: proto.String(".aliasparity.Holder"), Options: alias("manyX")},
				{Name: proto.String("by_key"), Number: proto.Int32(3), Type: msgT, Label: rep, TypeName: proto.String(".aliasparity.Root.ByKeyEntry")},
			}, NestedType: []*descriptorpb.DescriptorProto{{
				Name:    proto.String("ByKeyEntry"),
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				Field: []*descriptorpb.FieldDescriptorProto{
					f("key", 1, str, "", nil),
					f("value", 2, msgT, ".aliasparity.Holder", nil),
				},
			}}},
		},
	}
	fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	return fd
}

// generatedStandIn is the contract a generated marshaler keeps: the
// collapsed dialect, aliases left to restgw (the generator emits proto names
// and MarshalProtoAppend runs the alias rewrite over its output). Registered
// for the test types, it routes MarshalProto down the generated path.
func generatedStandIn(dst []byte, m proto.Message) ([]byte, error) {
	raw, err := Marshaller.Marshal(m)
	if err != nil {
		return dst, err
	}
	desc := m.ProtoReflect().Descriptor()
	if protojsonx.HasAnyOneof(desc) {
		if raw, err = protojsonx.CollapseOneofs(raw, desc, nil); err != nil {
			return dst, err
		}
	}
	return append(dst, raw...), nil
}

// One message, two encoders: the generated path and the reflective one must
// write the same bytes, aliases below a oneof included. Before the fix the
// generated path left every alias under a oneof key unapplied.
func TestAliasBelowOneof_GeneratedMatchesReflective(t *testing.T) {
	fd := aliasOneofFile(t)
	root := fd.Messages().ByName("Root")
	for _, name := range []protoreflect.FullName{"aliasparity.Root", "aliasparity.Holder", "aliasparity.Arm", "aliasparity.Inner", "aliasparity.Leaf"} {
		RegisterJSONMarshaler(name, generatedStandIn)
	}
	t.Cleanup(func() {
		for _, name := range []protoreflect.FullName{"aliasparity.Root", "aliasparity.Holder", "aliasparity.Arm", "aliasparity.Inner", "aliasparity.Leaf"} {
			delete(jsonMarshalers, name)
		}
	})
	cases := map[string]string{
		"message arm with aliased fields": `{"one":{"top":"t","arm":{"armLabel":"L","plain":"p"}}}`,
		"nested message in an arm":        `{"one":{"arm":{"leaf":{"leafNote":"n"}}}}`,
		"oneof nested in an arm":          `{"one":{"arm":{"inner":{"deep":{"leafNote":"deep"}}}}}`,
		"nested oneof, scalar arm":        `{"one":{"arm":{"inner":{"deepText":"x"}}}}`,
		"aliased arm field":               `{"one":{"aliasedArm":{"armLabel":"A"}}}`,
		"scalar arm":                      `{"one":{"bare":"b"}}`,
		"object WKT arm":                  `{"one":{"st":{"armLabel":"stays","leaf_note":"stays"}}}`,
		"unset oneof":                     `{"one":{"top":"t"}}`,
		"list of oneof messages":          `{"many":[{"arm":{"armLabel":"1"}},{"bare":"2"},{"aliasedArm":{"leaf":{"leafNote":"3"}}}]}`,
		"map of oneof messages":           `{"byKey":{"k":{"arm":{"armLabel":"m","leaf":{"leafNote":"n"}}}}}`,
	}
	for name, protoJSON := range cases {
		t.Run(name, func(t *testing.T) {
			m := dynamicpb.NewMessage(root)
			if err := protojson.Unmarshal([]byte(protoJSON), m); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			if lookupJSONMarshaler(root) == nil {
				t.Fatal("the stand-in is not registered: MarshalProto would not take the generated path")
			}
			generated, err := MarshalProto(m)
			if err != nil {
				t.Fatal(err)
			}
			reflective, err := MarshalProtoReflective(nil, m)
			if err != nil {
				t.Fatal(err)
			}
			if string(generated) != string(reflective) {
				t.Errorf("one message, two wires:\n generated:  %s\n reflective: %s", generated, reflective)
			}
		})
	}
}

// What the shared wire says, pinned: every alias is applied below a oneof
// (json-dialect.md §2 keeps the rewrite for every field), the discriminator
// names the arm by its proto name, and an object-WKT arm's
// keys are user data, never renamed.
func TestAliasBelowOneof_WireShape(t *testing.T) {
	fd := aliasOneofFile(t)
	root := fd.Messages().ByName("Root")
	for name, c := range map[string]struct{ in, want string }{
		"arm":          {`{"one":{"arm":{"armLabel":"L","leaf":{"leafNote":"n"}}}}`, `{"one":{"sel":{"armLabel":"L","leafy":{"leafNote":"n"},"w17_discriminator":"arm"}}}`},
		"aliased arm":  {`{"one":{"aliasedArm":{"armLabel":"A"}}}`, `{"one":{"sel":{"armLabel":"A","w17_discriminator":"aliased_arm"}}}`},
		"nested oneof": {`{"one":{"arm":{"inner":{"deep":{"leafNote":"d"}}}}}`, `{"one":{"sel":{"inner":{"pick":{"leafNote":"d","w17_discriminator":"deep"}},"w17_discriminator":"arm"}}}`},
		"Struct arm":   {`{"one":{"st":{"leaf_note":"stays"}}}`, `{"one":{"sel":{"leaf_note":"stays"}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			m := dynamicpb.NewMessage(root)
			if err := protojson.Unmarshal([]byte(c.in), m); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			got, err := MarshalProtoReflective(nil, m)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("wire:\n got:  %s\n want: %s", got, c.want)
			}
		})
	}
}
