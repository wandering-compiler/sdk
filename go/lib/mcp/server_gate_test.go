package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func toolNames(tools []mcp.Tool) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		out[t.Name] = true
	}
	return out
}

func TestFilterTools_NoGate_AllVisible(t *testing.T) {
	s := NewServer("t", "1", nil) // no PermResolver -> open surface
	tools := []mcp.Tool{{Name: "a"}, {Name: "b"}}
	if got := s.filterTools(context.Background(), tools); len(got) != 2 {
		t.Errorf("open surface: %d tools, want 2", len(got))
	}
}

func TestFilterTools_GatedByPerms(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetPermResolver(func(context.Context, ConnectionInfo) ([]int32, error) { return []int32{1}, nil })
	s.SetToolPerm("a", 1) // covered
	s.SetToolPerm("b", 2) // not covered
	// "open" left ungated.
	tools := []mcp.Tool{{Name: "a"}, {Name: "b"}, {Name: "open"}}

	got := toolNames(s.filterTools(context.Background(), tools))
	if !got["a"] || !got["open"] || got["b"] {
		t.Errorf("filter = %v, want {a, open} (b not covered)", got)
	}
}

func TestFilterTools_AuthError_FailsClosed(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetPermResolver(func(context.Context, ConnectionInfo) ([]int32, error) { return nil, fmt.Errorf("boom") })
	s.SetToolPerm("a", 1)
	tools := []mcp.Tool{{Name: "a"}, {Name: "open"}}

	got := toolNames(s.filterTools(context.Background(), tools))
	if got["a"] || !got["open"] {
		t.Errorf("fail-closed filter = %v, want only {open} (gated tools hidden on auth error)", got)
	}
}

func callTool(s *Server, name string, resolver PermResolver) (bool, error) {
	if resolver != nil {
		s.SetPermResolver(resolver)
	}
	invoked := false
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	_, err := s.CallUnary(context.Background(), req, &emptypb.Empty{},
		func(ctx context.Context, _ proto.Message) (proto.Message, error) {
			invoked = true
			return &emptypb.Empty{}, nil
		})
	return invoked, err
}

func TestCallUnary_Gate_DeniesWithoutPerm(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetToolPerm("danger", 9)
	invoked, err := callTool(s, "danger", func(context.Context, ConnectionInfo) ([]int32, error) { return []int32{1}, nil })
	if err == nil {
		t.Fatal("expected permission denied")
	}
	if invoked {
		t.Error("handler must NOT run when the gate denies")
	}
}

func TestCallUnary_Gate_AllowsWithPerm(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetToolPerm("danger", 9)
	invoked, err := callTool(s, "danger", func(context.Context, ConnectionInfo) ([]int32, error) { return []int32{9}, nil })
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if !invoked {
		t.Error("handler must run when the gate allows")
	}
}

func TestCallUnary_Gate_UngatedToolRunsRegardless(t *testing.T) {
	s := NewServer("t", "1", nil)
	// "open" has no SetToolPerm; resolver returns no perms.
	invoked, err := callTool(s, "open", func(context.Context, ConnectionInfo) ([]int32, error) { return nil, nil })
	if err != nil || !invoked {
		t.Errorf("ungated tool must run: err=%v invoked=%v", err, invoked)
	}
}

func TestCallUnary_NoResolver_NoGate(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetToolPerm("danger", 9) // perm recorded, but no resolver -> open surface
	invoked, err := callTool(s, "danger", nil)
	if err != nil || !invoked {
		t.Errorf("open surface must run any tool: err=%v invoked=%v", err, invoked)
	}
}
