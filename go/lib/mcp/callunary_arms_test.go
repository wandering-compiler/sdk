package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestCallUnary_GateResolverError — when the permission resolver itself errors
// for a gated tool, CallUnary fails closed with the wrapped auth error (it does
// not fall through to the handler).
func TestCallUnary_GateResolverError(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetToolPerm("danger", 9)
	s.SetPermResolver(func(context.Context, ConnectionInfo) ([]int32, error) {
		return nil, errors.New("resolver boom")
	})
	req := mcp.CallToolRequest{}
	req.Params.Name = "danger"
	_, err := s.CallUnary(context.Background(), req, &emptypb.Empty{},
		func(context.Context, proto.Message) (proto.Message, error) { return &emptypb.Empty{}, nil })
	if err == nil || !strings.Contains(err.Error(), "mcp auth") {
		t.Fatalf("want wrapped auth error, got %v", err)
	}
}

// TestCallUnary_DecodeError — arguments whose JSON type can't decode into the
// request proto surface a decode error (before the handler runs).
func TestCallUnary_DecodeError(t *testing.T) {
	s := NewServer("t", "1", nil)
	req := mcp.CallToolRequest{}
	req.Params.Name = "x"
	// value is a uint32 wire field; a string is not a valid JSON encoding for it.
	req.Params.Arguments = map[string]any{"value": "not-a-number"}
	invoked := false
	_, err := s.CallUnary(context.Background(), req, &wrapperspb.UInt32Value{},
		func(context.Context, proto.Message) (proto.Message, error) {
			invoked = true
			return &emptypb.Empty{}, nil
		})
	if err == nil || !strings.Contains(err.Error(), "decode arguments") {
		t.Fatalf("want decode error, got %v", err)
	}
	if invoked {
		t.Error("handler must not run when arguments fail to decode")
	}
}
