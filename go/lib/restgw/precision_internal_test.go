package restgw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// precisionDesc builds a request message whose JSON is reshaped on the way
// in: an aliased int64, a plain int64 and a oneof with an int64 arm.
func precisionDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	file := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("precision_req.proto"),
		Package:    proto.String("precisiontest"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"w17/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Ref"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ref_id"), Number: proto.Int32(1), Label: opt, Type: i64, JsonName: proto.String("refId")},
			}},
			{Name: proto.String("Req"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("account_id"), Number: proto.Int32(1), Label: opt, Type: i64, JsonName: proto.String("accountId"), Options: fieldOptionsWithAlias("account")},
				{Name: proto.String("plain_id"), Number: proto.Int32(2), Label: opt, Type: i64, JsonName: proto.String("plainId")},
				{Name: proto.String("ref"), Number: proto.Int32(3), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".precisiontest.Ref"), JsonName: proto.String("ref"), OneofIndex: proto.Int32(0)},
				{Name: proto.String("note"), Number: proto.Int32(4), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), JsonName: proto.String("note"), OneofIndex: proto.Int32(0)},
			}, OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("target")}}},
		},
	}
	return buildMessageDescriptor(t, file, "precisiontest.Req")
}

const (
	beyondFloat  = 9007199254740993 // 2^53 + 1: not representable as a float64
	beyondFloat2 = 9007199254740995
)

func checkPrecision(t *testing.T, m *dynamicpb.Message) {
	t.Helper()
	f := m.Descriptor().Fields()
	if v := m.Get(f.ByName("account_id")).Int(); v != beyondFloat {
		t.Errorf("aliased account_id = %d, want %d", v, int64(beyondFloat))
	}
	if v := m.Get(f.ByName("plain_id")).Int(); v != beyondFloat {
		t.Errorf("plain_id = %d, want %d", v, int64(beyondFloat))
	}
	ref := m.Get(f.ByName("ref")).Message()
	if v := ref.Get(ref.Descriptor().Fields().ByName("ref_id")).Int(); v != beyondFloat2 {
		t.Errorf("oneof arm ref_id = %d, want %d", v, int64(beyondFloat2))
	}
}

const precisionBody = `{"account":9007199254740993,"plain_id":9007199254740993,` +
	`"target":{"w17_discriminator":"ref","ref_id":9007199254740995}}`

// A 64-bit id sent as a JSON number reaches the handler unchanged, whether
// or not the message has an alias or a oneof. It used to lose everything past
// 2^53 on exactly those messages, while the same field on a plain message
// kept it — the dialect's reshaping decoded numbers into float64.
func TestUnmarshalProto_KeepsSixtyFourBitNumbersExact(t *testing.T) {
	desc := precisionDesc(t)
	m := dynamicpb.NewMessage(desc)
	if err := UnmarshalProto([]byte(precisionBody), m); err != nil {
		t.Fatalf("UnmarshalProto: %v", err)
	}
	checkPrecision(t, m)
}

func TestDecodeRequest_KeepsSixtyFourBitNumbersExact(t *testing.T) {
	desc := precisionDesc(t)
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(precisionBody))
	r.Header.Set("Content-Type", "application/json")
	m := dynamicpb.NewMessage(desc)
	if err := DecodeRequest(r, m); err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	checkPrecision(t, m)
}

// The alias restore alone (no oneof involved) keeps the literal too.
func TestRestoreAliasesOnRequestJSON_KeepsNumbersExact(t *testing.T) {
	desc := precisionDesc(t)
	out, err := restoreAliasesOnRequestJSON([]byte(`{"account":9007199254740993}`), desc)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Errorf("restored JSON changed the number: %s", out)
	}
}

// The response-side rewrite keeps every value protojson wrote.
func TestMarshalProto_ReshapedResponseKeepsValues(t *testing.T) {
	desc := precisionDesc(t)
	m := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(`{"account_id":"9007199254740993","plain_id":"-9007199254740993","note":"n"}`), m); err != nil {
		t.Fatal(err)
	}
	out, err := MarshalProto(m)
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}
	back := dynamicpb.NewMessage(desc)
	if err := UnmarshalProto(out, back); err != nil {
		t.Fatalf("UnmarshalProto(%s): %v", out, err)
	}
	if !proto.Equal(m, back) {
		t.Errorf("round trip changed a value:\n sent: %v\n back: %v\n json: %s", m, back, out)
	}
}
