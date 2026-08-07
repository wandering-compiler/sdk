package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// T2-6 pass #5 B1: an MCP tool must be able to require MORE than one
// permission, so the transport can enforce REST's full endpoint+model
// gate. With a single id the model perm was silently dropped and a
// token carrying only the endpoint perm could read the model through
// MCP while REST answered 403 (docs/decisions/rpc-transport-enforces-acl.md
// makes the same argument for the RPC transport, Q57-gateway-1).

func TestSetToolPerms_CallRequiresEveryPerm(t *testing.T) {
	s := NewServer("t", "1", nil)
	// Endpoint perm 7 + model perm 9, exactly the matrix-admin-acl-mcp
	// GetNote shape.
	s.SetToolPerms("get_note", 7, 9)

	// A token carrying ONLY the endpoint perm must be refused: this is
	// the bypass. Role globs like `include: ["example.NoteQuery.*"]`
	// produce exactly this token.
	ok, err := callTool(s, "get_note", func(context.Context, ConnectionInfo) ([]int32, error) {
		return []int32{7}, nil
	})
	if ok {
		t.Fatal("tool ran with endpoint perm only — model perm not enforced (ACL bypass)")
	}
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want permission denied", err)
	}

	// Both perms -> allowed.
	ok, err = callTool(s, "get_note", func(context.Context, ConnectionInfo) ([]int32, error) {
		return []int32{7, 9}, nil
	})
	if err != nil || !ok {
		t.Fatalf("full perms: ok=%v err=%v, want invoked", ok, err)
	}
}

func TestSetToolPerms_ListHidesToolMissingAnyPerm(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetPermResolver(func(context.Context, ConnectionInfo) ([]int32, error) {
		return []int32{7}, nil
	})
	s.SetToolPerms("get_note", 7, 9)

	// tools/list must hide it too — otherwise the tool is advertised to
	// the model and only fails on call.
	got := toolNames(s.filterTools(context.Background(), []mcp.Tool{{Name: "get_note"}}))
	if got["get_note"] {
		t.Error("tools/list advertised a tool whose model perm the token lacks")
	}
}

// SetToolPerm stays a single-id wrapper so existing generated callers
// keep compiling and behaving identically.
func TestSetToolPerm_SingleIDUnchanged(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetToolPerm("a", 1)

	ok, err := callTool(s, "a", func(context.Context, ConnectionInfo) ([]int32, error) {
		return []int32{1}, nil
	})
	if err != nil || !ok {
		t.Fatalf("single-perm tool: ok=%v err=%v, want invoked", ok, err)
	}
	// Perm 0 stays ungated.
	s.SetToolPerms("zero", 0)
	if _, gated := s.toolPerms["zero"]; gated {
		t.Error("perm id 0 recorded a gate; want ungated")
	}
}
