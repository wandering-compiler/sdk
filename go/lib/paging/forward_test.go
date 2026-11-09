package paging_test

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/paging"
)

func outgoing(t *testing.T, ctx context.Context) metadata.MD {
	t.Helper()
	md, _ := metadata.FromOutgoingContext(ctx)
	return md
}

func TestForward_CopiesIncomingPagingOntoOutgoing(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		paging.BoundariesMDKey, "b64cursor",
		paging.LimitMDKey, "25",
		paging.TotalMDKey, "1",
		"x-w17-user", "envelope", // not paging: Forward must not move it
	))
	out := outgoing(t, paging.Forward(ctx))
	for k, want := range map[string]string{
		paging.BoundariesMDKey: "b64cursor",
		paging.LimitMDKey:      "25",
		paging.TotalMDKey:      "1",
	} {
		if got := out.Get(k); !slices.Equal(got, []string{want}) {
			t.Errorf("%s = %v, want [%s]", k, got, want)
		}
	}
	if got := out.Get("x-w17-user"); len(got) != 0 {
		t.Errorf("x-w17-user forwarded (%v): paging.Forward moves paging only", got)
	}
}

func TestForward_ReplacesStaleOutgoingPagingAsASet(t *testing.T) {
	// A context reused from an earlier page: boundaries and a limit already
	// outgoing. The new request is a FIRST page — a limit, no boundaries — so
	// the stale boundaries must not survive, or storage serves page 2 again.
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		paging.BoundariesMDKey, "stale",
		paging.LimitMDKey, "10",
		"w17-language", "cs",
	))
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(paging.LimitMDKey, "50"))
	out := outgoing(t, paging.Forward(ctx))
	if got := out.Get(paging.LimitMDKey); !slices.Equal(got, []string{"50"}) {
		t.Errorf("limit = %v, want [50] — the incoming value replaces, never appends", got)
	}
	if got := out.Get(paging.BoundariesMDKey); len(got) != 0 {
		t.Errorf("stale boundaries survived: %v", got)
	}
	if got := out.Get("w17-language"); !slices.Equal(got, []string{"cs"}) {
		t.Errorf("an unrelated outgoing key was lost: %v", got)
	}
}

func TestForward_NothingWhenIncomingCarriesNoPaging(t *testing.T) {
	base := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("k", "v"))
	ctx := metadata.NewIncomingContext(base, metadata.Pairs("x-w17-user", "e"))
	if got := paging.Forward(ctx); got != ctx {
		t.Error("Forward returned a new context with nothing to forward")
	}
	bare := context.Background()
	if got := paging.Forward(bare); got != bare {
		t.Error("Forward changed a context with no incoming metadata")
	}
}

func TestForward_DoesNotWriteThroughToTheParentsOutgoingMetadata(t *testing.T) {
	parentMD := metadata.Pairs("k", "v")
	ctx := metadata.NewOutgoingContext(context.Background(), parentMD)
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(paging.LimitMDKey, "5"))
	_ = paging.Forward(ctx)
	if len(parentMD.Get(paging.LimitMDKey)) != 0 {
		t.Error("Forward mutated the parent's outgoing MD in place — a sibling call would inherit the page")
	}
}
