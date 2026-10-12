package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// argsDesc is a tool request with a 64-bit id beside a oneof (the shape the
// dialect reshapes) and one without any reshaping at all.
func argsDesc(t *testing.T) (withOneof, plain protoreflect.MessageDescriptor) {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("mcp_args_test.proto"),
		Package: proto.String("mcpargs"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("WithOneof"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Label: opt, Type: i64},
				{Name: proto.String("a"), Number: proto.Int32(2), Label: opt, Type: str, OneofIndex: proto.Int32(0)},
				{Name: proto.String("b"), Number: proto.Int32(3), Label: opt, Type: i64, OneofIndex: proto.Int32(0)},
			}, OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("pick")}}},
			{Name: proto.String("Plain"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Label: opt, Type: i64},
			}},
		},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{file}})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := files.FindDescriptorByName("mcpargs.WithOneof")
	p, _ := files.FindDescriptorByName("mcpargs.Plain")
	return w.(protoreflect.MessageDescriptor), p.(protoreflect.MessageDescriptor)
}

// idTool registers a tool decoding into desc and records the id it saw.
func idTool(s *Server, name string, desc protoreflect.MessageDescriptor, seen *sync.Map) {
	s.AddTool(name, "d", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return s.CallUnary(ctx, req, dynamicpb.NewMessage(desc), func(_ context.Context, m proto.Message) (proto.Message, error) {
			pm := m.ProtoReflect()
			seen.Store(name, pm.Get(pm.Descriptor().Fields().ByName("id")).Int())
			return &emptypb.Empty{}, nil
		})
	})
}

const exactID = 9007199254740993 // 2^53 + 1: a float64 cannot hold it

// A 64-bit id sent as a JSON number reaches the backend unchanged over the
// HTTP transport, with or without a oneof in the message. The library
// decodes tool arguments into float64, which turned it into
// 9007199254740992 before CallUnary ever saw it.
func TestServeStreamableHTTP_ToolArgumentsKeepSixtyFourBitNumbers(t *testing.T) {
	withOneof, plain := argsDesc(t)
	s := NewServer("x", "1", nil)
	var seen sync.Map
	idTool(s, "oneof", withOneof, &seen)
	idTool(s, "plain", plain, &seen)

	base := startHTTP(t, s)
	url := base + EndpointPath
	resp, body := postMCP(t, url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: %d %s", resp.StatusCode, body)
	}
	session := resp.Header.Get("Mcp-Session-Id")
	_, _ = postMCP(t, url, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	for _, tc := range []struct{ tool, args string }{
		{"oneof", `{"id":9007199254740993,"pick":"x"}`},
		{"plain", `{"id":9007199254740993}`},
	} {
		_, body = postMCP(t, url, session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+tc.tool+`","arguments":`+tc.args+`}}`)
		if !strings.Contains(body, `"result"`) {
			t.Fatalf("%s: tool call failed: %s", tc.tool, body)
		}
		got, _ := seen.Load(tc.tool)
		if got != int64(exactID) {
			t.Errorf("%s: backend received id %v, want %d", tc.tool, got, int64(exactID))
		}
	}
}

// The stdio transport hands every message the connection's scoped context;
// the same capture applies there.
func TestHandleMessage_ScopedContextKeepsSixtyFourBitNumbers(t *testing.T) {
	withOneof, _ := argsDesc(t)
	s := NewServer("x", "1", nil)
	var seen sync.Map
	idTool(s, "oneof", withOneof, &seen)

	ctx := withArgScope(context.Background())
	resp := s.Inner().HandleMessage(ctx, json.RawMessage(
		`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"oneof","arguments":{"id":9007199254740993,"pick":9007199254740995}}}`))
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"result"`) {
		t.Fatalf("tool call failed: %s", b)
	}
	if got, _ := seen.Load("oneof"); got != int64(exactID) {
		t.Errorf("backend received id %v, want %d", got, int64(exactID))
	}
	if n := len(argScopeFrom(ctx).byID); n != 0 {
		t.Errorf("%d captured argument sets left behind after the call", n)
	}
}

// A tools/call the library refuses before the tool runs leaves nothing
// behind in the connection's scope.
func TestHandleMessage_RefusedCallLeavesNoCapture(t *testing.T) {
	s := NewServer("x", "1", nil)
	s.AddTool("t", "d", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	ctx := withArgScope(context.Background())
	for _, msg := range []string{
		// params.name has the wrong type: the library cannot decode the request.
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":5,"arguments":{"id":1}}}`,
		// unknown tool: refused by the library after the before-call hook.
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"nope","arguments":{"id":1}}}`,
	} {
		_ = s.Inner().HandleMessage(ctx, json.RawMessage(msg))
	}
	if n := len(argScopeFrom(ctx).byID); n != 0 {
		t.Errorf("%d captured argument sets left behind by refused calls", n)
	}
}
