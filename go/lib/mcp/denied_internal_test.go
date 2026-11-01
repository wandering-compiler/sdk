package mcp

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/wandering-compiler/sdk/go/lib/acllock"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

func callWithScopes(t *testing.T, held []int32, scopes map[string]string) error {
	t.Helper()
	s := NewServer("t", "1", nil)
	s.SetToolPerm("areas", 7)
	s.SetPermScopesResolver(func(context.Context, ConnectionInfo) ([]int32, map[string]string, error) {
		return held, scopes, nil
	})
	invoked, err := callTool(s, "areas", nil)
	if invoked {
		t.Error("the handler ran although the gate refused")
	}
	return err
}

func TestCallUnary_Gate_UnnamedOrganizationIsOrganizationRequired(t *testing.T) {
	err := callWithScopes(t, []int32{1}, map[string]string{acllock.OrgPermissionsScope: "7"})
	st := status.Convert(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition: %v", st.Code(), err)
	}
	found := false
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok && ed.GetCode() == "ORGANIZATION_REQUIRED" {
			found = true
		}
	}
	if !found {
		t.Errorf("no ORGANIZATION_REQUIRED detail: %v", st.Details())
	}
	if !strings.Contains(st.Message(), "W17-Org") {
		t.Errorf("message = %q, want the sentence naming W17-Org", st.Message())
	}
	if strings.Contains(st.Message(), "areas") {
		t.Errorf("the operator's copy reached the caller: %q", st.Message())
	}
}

func TestCallUnary_Gate_WithoutTheHintStaysPermissionDenied(t *testing.T) {
	for name, scopes := range map[string]map[string]string{
		"none":         nil,
		"another id":   {acllock.OrgPermissionsScope: "8"},
		"untranslated": {acllock.OrgProductRolesScope: "7"},
	} {
		if c := status.Code(callWithScopes(t, []int32{1}, scopes)); c != codes.PermissionDenied {
			t.Errorf("%s: code = %s, want PermissionDenied", name, c)
		}
	}
}

// The scopes resolver also backs the list filter, so one installation gates
// both halves.
func TestSetPermScopesResolver_BacksTheListFilter(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetToolPerm("a", 1)
	s.SetToolPerm("b", 2)
	s.SetPermScopesResolver(func(context.Context, ConnectionInfo) ([]int32, map[string]string, error) {
		return []int32{1}, nil, nil
	})
	got := toolNames(s.filterTools(context.Background(), []mcp.Tool{{Name: "a"}, {Name: "b"}}))
	if !got["a"] || got["b"] {
		t.Errorf("filter = %v, want {a}", got)
	}
}
