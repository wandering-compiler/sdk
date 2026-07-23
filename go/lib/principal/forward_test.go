package principal_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// TestForwardToOutgoing_RelaysPrincipal pins the core relay: the
// gateway-trusted principal read from a handler's INCOMING metadata is
// re-emitted as OUTGOING metadata so a downstream (storage) tier reached
// over a real gRPC hop sees it via FromIncomingContext — the exact key the
// generated scope guard reads.
func TestForwardToOutgoing_RelaysPrincipal(t *testing.T) {
	in := metadata.Pairs(
		"x-w17-user", "envelope-bytes",
		"x-w17-scope-org_id", "org-1",
		"x-w17-scope-user_id", "u-9",
	)
	ctx := principal.ForwardToOutgoing(metadata.NewIncomingContext(context.Background(), in))

	out, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("no outgoing metadata produced")
	}
	for k, want := range map[string]string{
		"x-w17-user":          "envelope-bytes",
		"x-w17-scope-org_id":  "org-1",
		"x-w17-scope-user_id": "u-9",
	} {
		if got := out.Get(k); len(got) != 1 || got[0] != want {
			t.Errorf("outgoing[%q] = %v, want [%q]", k, got, want)
		}
	}
}

// TestForwardToOutgoing_ExcludesNonPrincipal locks the scope of the relay:
// only the principal is forwarded. Paging metadata is request-direction and
// query-specific — forwarding it would bleed one query's LIMIT/keyset onto
// sibling storage calls — and tracing / i18n / tx-routing / auth headers
// have their own propagation. All must stay put.
func TestForwardToOutgoing_ExcludesNonPrincipal(t *testing.T) {
	in := metadata.Pairs(
		"x-w17-user", "env",
		"x-w17-paging-limit", "50",
		"x-w17-paging-boundaries", "abc",
		"w17-language", "cs",
		"w17-tx-id", "tx-1",
		"authorization", "bearer zzz",
	)
	ctx := principal.ForwardToOutgoing(metadata.NewIncomingContext(context.Background(), in))
	out, _ := metadata.FromOutgoingContext(ctx)

	if got := out.Get("x-w17-user"); len(got) != 1 || got[0] != "env" {
		t.Errorf("x-w17-user should be forwarded, got %v", got)
	}
	for _, k := range []string{
		"x-w17-paging-limit", "x-w17-paging-boundaries",
		"w17-language", "w17-tx-id", "authorization",
	} {
		if got := out.Get(k); len(got) != 0 {
			t.Errorf("outgoing[%q] = %v, want it excluded from the relay", k, got)
		}
	}
}

// TestForwardToOutgoing_PreservesExplicitOutgoing pins idempotence and the
// override rule: a value the handler deliberately set on the OUTGOING side
// (e.g. a cross-tenant call) wins and is never duplicated by the relay.
func TestForwardToOutgoing_PreservesExplicitOutgoing(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-w17-scope-org_id", "from-caller"))
	ctx = metadata.AppendToOutgoingContext(ctx, "x-w17-scope-org_id", "override")

	ctx = principal.ForwardToOutgoing(ctx)

	out, _ := metadata.FromOutgoingContext(ctx)
	if got := out.Get("x-w17-scope-org_id"); len(got) != 1 || got[0] != "override" {
		t.Errorf("outgoing scope = %v, want single [override] (explicit value preserved, not duplicated)", got)
	}
}

// TestForwardToOutgoing_NoIncomingIsNoop confirms an unauthenticated /
// gateway-bypassed call (no incoming principal) yields no outgoing metadata,
// so a downstream scope guard still fails closed rather than seeing a forged
// or empty scope.
func TestForwardToOutgoing_NoIncomingIsNoop(t *testing.T) {
	ctx := principal.ForwardToOutgoing(context.Background())
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		t.Errorf("expected no outgoing metadata without an incoming principal, got %v", md)
	}
}
