package restgw_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// A key the message does not declare is dropped, and the fields it does
// declare still decode: an older server keeps serving a newer client.
func TestDecodeRequest_IgnoresUnknownFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"orders.proto","addedInV2":{"deep":[1,2]},"package":"shop"}`))
	got := &descriptorpb.FileDescriptorProto{}
	if err := restgw.DecodeRequest(req, got); err != nil {
		t.Fatalf("DecodeRequest = %v, want unknown fields ignored", err)
	}
	if got.GetName() != "orders.proto" || got.GetPackage() != "shop" {
		t.Errorf("decoded %v, want the declared fields kept", got)
	}
}

// A body that does not decode is answered with the canonical 400; the
// decoder's own prose — which names types and offsets — stays on the
// operator's channel.
func TestWriteInvalidRequest_KeepsDecoderProseFromTheClient(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name": 17}`))
	err := restgw.DecodeRequest(req, &descriptorpb.FileDescriptorProto{})
	if err == nil {
		t.Fatal("a mistyped field decoded")
	}
	rec := httptest.NewRecorder()
	restgw.WriteInvalidRequest(context.Background(), rec, "decode request: "+err.Error())

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	for _, prose := range []string{err.Error(), "decode request", "name"} {
		if strings.Contains(body, prose) {
			t.Errorf("response carries the decoder's %q: %s", prose, body)
		}
	}
}

// Enums go out as their numbers on the REST response path.
func TestWriteResponse_EnumsAreNumbers(t *testing.T) {
	rec := httptest.NewRecorder()
	msg := &descriptorpb.FieldDescriptorProto{Name: proto.String("sku"), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()}
	restgw.WriteResponse(rec, http.StatusOK, msg, httptest.NewRequest(http.MethodGet, "/", nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if v, ok := got["type"].(float64); !ok || v != float64(descriptorpb.FieldDescriptorProto_TYPE_STRING) {
		t.Errorf("type = %#v, want the number %d", got["type"], descriptorpb.FieldDescriptorProto_TYPE_STRING)
	}
}

// The encoders the generated fast path emits well-known types with produce
// the same JSON value as the reflective path for the same message.
func TestFastPathEncoders_MatchTheReflectivePath(t *testing.T) {
	st, _ := structpb.NewStruct(map[string]any{"a": 1.5, "b": []any{"x", true, nil}, "c": map[string]any{"d": "e"}})
	inner, _ := anypb.New(wrapperspb.String("hi"))
	cases := []struct {
		name string
		msg  proto.Message
	}{
		{"Timestamp", timestamppb.New(time.Date(2026, 7, 26, 9, 5, 3, 120000000, time.UTC))},
		{"Timestamp whole second", timestamppb.New(time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC))},
		{"Duration", durationpb.New(-1500 * time.Millisecond)},
		{"Duration nanos", durationpb.New(3*time.Second + 7)},
		{"Struct", st},
		{"Value", structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{structpb.NewNumberValue(2), structpb.NewStringValue("z")}})},
		{"ListValue", &structpb.ListValue{Values: []*structpb.Value{structpb.NewBoolValue(false), structpb.NewNullValue()}}},
		{"FieldMask", &fieldmaskpb.FieldMask{Paths: []string{"display_name", "address.postal_code"}}},
		{"Any", inner},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var fast []byte
			var err error
			switch m := c.msg.(type) {
			case *timestamppb.Timestamp:
				fast = restgw.AppendTimestampJSON(nil, m)
			case *durationpb.Duration:
				fast = restgw.AppendDurationJSON(nil, m)
			case *structpb.Struct:
				fast = restgw.AppendStructJSON(nil, m)
			case *structpb.Value:
				fast = restgw.AppendValueJSON(nil, m)
			case *structpb.ListValue:
				fast = restgw.AppendListValueJSON(nil, m)
			case *fieldmaskpb.FieldMask:
				fast = restgw.AppendFieldMaskJSON(nil, m)
			case *anypb.Any:
				fast, err = restgw.AppendAnyJSON(nil, m)
			}
			if err != nil {
				t.Fatal(err)
			}
			slow, err := restgw.Marshaller.Marshal(c.msg)
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			if err := json.Unmarshal(fast, &a); err != nil {
				t.Fatalf("fast path %q is not JSON: %v", fast, err)
			}
			if err := json.Unmarshal(slow, &b); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("fast path %s, reflective path %s", fast, slow)
			}
		})
	}
}
