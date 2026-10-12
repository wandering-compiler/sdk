package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// A tool result is encoded like every other w17 JSON surface: proto field
// names and enums as their numbers.
func TestCallUnary_ResultUsesProtoNamesAndIntegerEnums(t *testing.T) {
	s := NewServer("t", "1", nil)
	reply := &descriptorpb.FieldDescriptorProto{
		Name:     proto.String("title"),
		JsonName: proto.String("title"),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
	}
	res, err := s.CallUnary(context.Background(), mcp.CallToolRequest{}, &structpb.Struct{}, func(context.Context, proto.Message) (proto.Message, error) {
		return reply, nil
	})
	if err != nil || res == nil || res.IsError || len(res.Content) == 0 {
		t.Fatalf("CallUnary = %+v, %v", res, err)
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %T, want text", res.Content[0])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text.Text), &got); err != nil {
		t.Fatalf("result is not JSON: %v: %s", err, text.Text)
	}
	if v, ok := got["type"].(float64); !ok || v != 9 {
		t.Errorf("enum not encoded as its number: type = %#v in %s", got["type"], text.Text)
	}
	if _, ok := got["json_name"]; !ok {
		t.Errorf("field not encoded under its proto name: %s", text.Text)
	}
}
