package principal_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	authpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/auth"
)

// The caller's map must not alias either installed side: a later Set on the
// md the caller still holds must not rewrite what the gateway installed.
func TestWithGatewayMD_TheCallersMapDoesNotAliasEitherSide(t *testing.T) {
	md := metadata.Pairs("x-w17-user", "verified")
	ctx := principal.WithGatewayMD(context.Background(), md)
	md.Set("x-w17-user", "mutated-through-the-callers-map")
	in, _ := metadata.FromIncomingContext(ctx)
	out, _ := metadata.FromOutgoingContext(ctx)
	if v := in.Get("x-w17-user"); len(v) != 1 || v[0] != "verified" {
		t.Errorf("mutating the caller's map changed the incoming side: %v", v)
	}
	if v := out.Get("x-w17-user"); len(v) != 1 || v[0] != "verified" {
		t.Errorf("mutating the caller's map changed the outgoing side: %v", v)
	}
}

// The relay triggers on ANY incoming relayed key, not only on a principal:
// an incoming x-w17-host alone replaces the outgoing relayed set, so the
// gateway's outgoing principal stamps on a context that also carries an
// incoming relayed key are dropped. A context with no incoming relayed key
// is returned unchanged.
func TestForwardToOutgoing_AnyIncomingRelayedKeyTriggersTheReplacement(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-w17-host", "app.example"))
	ctx = metadata.AppendToOutgoingContext(ctx, "x-w17-user", "stale-envelope", "x-w17-scope-org_id", "org-stale", "x-trace", "t-1")
	out, _ := metadata.FromOutgoingContext(principal.ForwardToOutgoing(ctx))
	if v := out.Get("x-w17-host"); len(v) != 1 || v[0] != "app.example" {
		t.Errorf("x-w17-host = %v, want the incoming value relayed", v)
	}
	if v := out.Get("x-w17-user"); len(v) != 0 {
		t.Errorf("x-w17-user = %v; an incoming relayed key replaces the outgoing relayed set", v)
	}
	if v := out.Get("x-w17-scope-org_id"); len(v) != 0 {
		t.Errorf("x-w17-scope-org_id = %v; dropped with the relayed set", v)
	}
	if v := out.Get("x-trace"); len(v) != 1 {
		t.Errorf("x-trace = %v; a key outside the relayed set is kept", v)
	}

	plain := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-trace", "t-1"))
	plain = metadata.AppendToOutgoingContext(plain, "x-w17-user", "gateway-stamp")
	if got := principal.ForwardToOutgoing(plain); got != plain {
		t.Error("a context with no incoming relayed key must be returned unchanged")
	}
}

// Keys are compared folded on both sides: an incoming map built in-process
// with a mixed-case key (grpc-go lowercases only what crosses the wire) is
// relayed like its lowercase spelling, as the outgoing side already was.
func TestForwardToOutgoing_FoldsTheIncomingKey(t *testing.T) {
	in := metadata.MD{"X-W17-User": {"envelope"}, "X-W17-Scope-Org_id": {"org-1"}}
	out, _ := metadata.FromOutgoingContext(principal.ForwardToOutgoing(metadata.NewIncomingContext(context.Background(), in)))
	if v := out.Get("x-w17-user"); len(v) != 1 || v[0] != "envelope" {
		t.Errorf("x-w17-user = %v, want the mixed-case incoming key relayed", v)
	}
	if v := out.Get("x-w17-scope-org_id"); len(v) != 1 || v[0] != "org-1" {
		t.Errorf("x-w17-scope-org_id = %v, want the mixed-case incoming key relayed", v)
	}
}

// An empty x-w17-user value is no principal: Envelope fails closed and
// leaves dst exactly as the caller supplied it.
func TestEnvelope_FailsClosedOnAnEmptyValueAndLeavesDst(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no metadata": context.Background(),
		"empty value": metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-w17-user", "")),
	} {
		t.Run(name, func(t *testing.T) {
			dst := &authpb.SignInResp{UserId: "untouched"}
			before := proto.Clone(dst)
			if err := principal.Envelope(ctx, dst); err == nil {
				t.Fatal("Envelope succeeded without a principal")
			}
			if !proto.Equal(dst, before) {
				t.Errorf("dst = %v, want it untouched (%v)", dst, before)
			}
			if uid, err := principal.UserID(ctx, &authpb.SignInResp{}); err == nil || uid != "" {
				t.Errorf("UserID = %q, %v; want a fail-closed empty id", uid, err)
			}
		})
	}
}

// ActAs stamps one principal: a user_id scope that names someone other than
// the envelope's user is refused, and so is a scope name the scope readers
// and the wire do not accept.
func TestActAs_RefusesAScopeThatDisagreesWithTheUserOrIsNotAName(t *testing.T) {
	user := &authpb.SignInResp{UserId: "u-env"}
	if _, err := principal.ActAs(context.Background(), user, map[string]string{"user_id": "u-scope"}); err == nil {
		t.Error("ActAs accepted a user_id scope naming another user than the envelope")
	}
	if _, err := principal.ActAs(context.Background(), user, map[string]string{"user_id": "u-env"}); err != nil {
		t.Errorf("ActAs refused a user_id scope that agrees with the envelope: %v", err)
	}
	for _, name := range []string{"org id", "Org_ID", "org-id", "org.id", "x\n"} {
		if _, err := principal.ActAs(context.Background(), user, map[string]string{name: "v"}); err == nil {
			t.Errorf("ActAs accepted the scope name %q", name)
		}
	}
	if _, err := principal.ActAs(context.Background(), user, map[string]string{"org_id2": "v"}); err != nil {
		t.Errorf("ActAs refused a valid scope name: %v", err)
	}
}
