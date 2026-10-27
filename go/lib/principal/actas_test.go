package principal_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	authpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/auth"
)

// callee is what the downstream tier sees: the caller's outgoing metadata
// arrives as its incoming metadata.
func callee(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	out, _ := metadata.FromOutgoingContext(ctx)
	return metadata.NewIncomingContext(context.Background(), out)
}

func TestActAs_TheCalleeSeesTheUserAndScopes(t *testing.T) {
	ctx, err := principal.ActAs(context.Background(), &authpb.SignInResp{UserId: "u-ana"},
		map[string]string{"org_id": "org-7"})
	if err != nil {
		t.Fatal(err)
	}
	in := callee(t, ctx)
	if uid, err := principal.UserID(in, &authpb.SignInResp{}); err != nil || uid != "u-ana" {
		t.Fatalf("UserID = %q, %v; want u-ana", uid, err)
	}
	if org, ok := principal.Scope(in, "org_id"); !ok || org != "org-7" {
		t.Fatalf("Scope(org_id) = %q, %v; want org-7", org, ok)
	}
}

// A context already carrying someone — a gateway's principal on the incoming
// side, stale stamps on the outgoing side — calls as the user ActAs names and
// nobody else, and a later ForwardToOutgoing does not put the old one back.
func TestActAs_ReplacesEveryOtherPrincipal(t *testing.T) {
	base := envelopeCtx(t, &authpb.SignInResp{UserId: "u-gateway"},
		"x-w17-scope-org_id", "org-gateway", "x-w17-scope-tenant_id", "t-gateway", "w17-label-team", "a")
	base = metadata.NewOutgoingContext(base, metadata.Pairs(
		"x-w17-scope-tenant_id", "t-stale", "x-request-id", "r-1"))

	ctx, err := principal.ActAs(base, &authpb.SignInResp{UserId: "u-ana"},
		map[string]string{"org_id": "org-7"})
	if err != nil {
		t.Fatal(err)
	}
	ctx = principal.ForwardToOutgoing(ctx)
	in := callee(t, ctx)

	if uid, _ := principal.UserID(in, &authpb.SignInResp{}); uid != "u-ana" {
		t.Fatalf("UserID = %q; want u-ana", uid)
	}
	if org, _ := principal.Scope(in, "org_id"); org != "org-7" {
		t.Fatalf("Scope(org_id) = %q; want org-7", org)
	}
	if v, ok := principal.Scope(in, "tenant_id"); ok {
		t.Fatalf("a scope ActAs was not given travelled: tenant_id = %q", v)
	}
	if v, ok := principal.Label(in, "team"); ok {
		t.Fatalf("the gateway's label travelled: team = %q", v)
	}
	if got := metadata.ValueFromIncomingContext(in, "x-request-id"); len(got) != 1 || got[0] != "r-1" {
		t.Fatalf("a non-principal outgoing key was lost: %v", got)
	}
}

func TestActAs_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		user   principal.UserIDMessage
		scopes map[string]string
	}{
		"no user":       {nil, nil},
		"empty user id": {&authpb.SignInResp{}, nil},
		"empty scope":   {&authpb.SignInResp{UserId: "u"}, map[string]string{"org_id": ""}},
		"unnamed scope": {&authpb.SignInResp{UserId: "u"}, map[string]string{"": "x"}},
		"padded scope":  {&authpb.SignInResp{UserId: "u"}, map[string]string{" org_id": "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := principal.ActAs(context.Background(), tc.user, tc.scopes); err == nil {
				t.Fatal("ActAs accepted it")
			}
		})
	}
}
