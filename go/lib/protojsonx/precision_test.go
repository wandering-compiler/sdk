package protojsonx_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/lib/protojsonx"
)

// buildNumericDesc compiles a message N with 64-bit and floating fields next
// to a genuine oneof, the shape whose request JSON goes through ExpandOneofs.
func buildNumericDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	typ := func(k descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto_Type {
		return k.Enum()
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("px_numeric_test.proto"),
		Package: proto.String("pxnum"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Inner"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Label: opt, Type: typ(descriptorpb.FieldDescriptorProto_TYPE_INT64)},
			}},
			{Name: proto.String("N"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("big"), Number: proto.Int32(1), Label: opt, Type: typ(descriptorpb.FieldDescriptorProto_TYPE_INT64)},
				{Name: proto.String("ubig"), Number: proto.Int32(2), Label: opt, Type: typ(descriptorpb.FieldDescriptorProto_TYPE_UINT64)},
				{Name: proto.String("ids"), Number: proto.Int32(3), Label: rep, Type: typ(descriptorpb.FieldDescriptorProto_TYPE_SINT64)},
				{Name: proto.String("ratio"), Number: proto.Int32(4), Label: opt, Type: typ(descriptorpb.FieldDescriptorProto_TYPE_DOUBLE)},
				{Name: proto.String("inner"), Number: proto.Int32(5), Label: opt, Type: typ(descriptorpb.FieldDescriptorProto_TYPE_MESSAGE), TypeName: proto.String(".pxnum.Inner"), OneofIndex: proto.Int32(0)},
				{Name: proto.String("label"), Number: proto.Int32(6), Label: opt, Type: typ(descriptorpb.FieldDescriptorProto_TYPE_STRING), OneofIndex: proto.Int32(0)},
			}, OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("pick")}}},
		},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{file}})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("pxnum.N")
	if err != nil {
		t.Fatalf("find N: %v", err)
	}
	return d.(protoreflect.MessageDescriptor)
}

// Reshaping a request must not change any value it carries. 64-bit integers
// sent as JSON numbers (protojson accepts them unquoted) used to round-trip
// through float64 and come out as different numbers — only for messages with
// a oneof, because only those are reshaped.
func TestExpand_KeepsNumbersExact(t *testing.T) {
	desc := buildNumericDesc(t)
	in := `{"big":9007199254740993,"ubig":18446744073709551615,"ids":[-9007199254740993,9223372036854775807],` +
		`"ratio":0.30000000000000004,"pick":{"w17_discriminator":"inner","id":9007199254740995}}`

	expanded, err := protojsonx.ExpandOneofs([]byte(in), desc, nil)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	got := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal(expanded, got); err != nil {
		t.Fatalf("decode %s: %v", expanded, err)
	}

	f := desc.Fields()
	if v := got.Get(f.ByName("big")).Int(); v != 9007199254740993 {
		t.Errorf("big = %d, want 9007199254740993 (expanded: %s)", v, expanded)
	}
	if v := got.Get(f.ByName("ubig")).Uint(); v != 18446744073709551615 {
		t.Errorf("ubig = %d, want 18446744073709551615", v)
	}
	ids := got.Get(f.ByName("ids")).List()
	if ids.Len() != 2 || ids.Get(0).Int() != -9007199254740993 || ids.Get(1).Int() != 9223372036854775807 {
		t.Errorf("ids = %v, want [-9007199254740993 9223372036854775807]", ids)
	}
	if v := got.Get(f.ByName("ratio")).Float(); v != 0.30000000000000004 {
		t.Errorf("ratio = %v, want 0.30000000000000004", v)
	}
	inner := got.Get(f.ByName("inner")).Message()
	if v := inner.Get(inner.Descriptor().Fields().ByName("id")).Int(); v != 9007199254740995 {
		t.Errorf("oneof arm id = %d, want 9007199254740995", v)
	}

	// The same values protojson decodes from the flat form directly: the
	// reshape is a key rewrite and nothing else.
	direct := dynamicpb.NewMessage(desc)
	flat := `{"big":9007199254740993,"ubig":18446744073709551615,"ids":[-9007199254740993,9223372036854775807],` +
		`"ratio":0.30000000000000004,"inner":{"id":9007199254740995}}`
	if err := protojson.Unmarshal([]byte(flat), direct); err != nil {
		t.Fatalf("direct decode: %v", err)
	}
	if !proto.Equal(got, direct) {
		t.Errorf("reshaped decode differs from protojson's own:\n got:    %v\n direct: %v", got, direct)
	}
}

// The response side keeps every number's spelling too.
func TestCollapse_KeepsNumbersExact(t *testing.T) {
	desc := buildNumericDesc(t)
	in := `{"ratio":1e+300,"ids":["-9007199254740993"],"label":"x","big":"9007199254740993"}`
	out, err := protojsonx.CollapseOneofs([]byte(in), desc, nil)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	got := dynamicpb.NewMessage(desc)
	expanded, err := protojsonx.ExpandOneofs(out, desc, nil)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if err := protojson.Unmarshal(expanded, got); err != nil {
		t.Fatalf("decode %s: %v", expanded, err)
	}
	want := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(in), want); err != nil {
		t.Fatalf("decode input: %v", err)
	}
	if !proto.Equal(got, want) {
		t.Errorf("collapse+expand changed a value:\n got:  %v\n want: %v", got, want)
	}
}

// DecodeTree keeps json.Unmarshal's whole-document rule: anything after the
// value is an error, so ExpandOneofs hands such a body back untouched and
// protojson reports it.
func TestDecodeTree_RejectsTrailingData(t *testing.T) {
	for _, in := range []string{`{} {}`, `{"a":1} ]`, `1 2`} {
		if _, err := protojsonx.DecodeTree([]byte(in)); err == nil {
			t.Errorf("DecodeTree(%q) accepted trailing data", in)
		}
	}
	for _, in := range []string{`{"a":1}`, " {\"a\":1} \n", `null`} {
		if _, err := protojsonx.DecodeTree([]byte(in)); err != nil {
			t.Errorf("DecodeTree(%q) = %v, want ok", in, err)
		}
	}
	desc := buildNumericDesc(t)
	bad := []byte(`{"big":1} {"big":2}`)
	out, err := protojsonx.ExpandOneofs(bad, desc, nil)
	if err != nil || string(out) != string(bad) {
		t.Errorf("ExpandOneofs on trailing data = %q, %v; want the input unchanged", out, err)
	}
}
