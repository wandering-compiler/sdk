package paging_test

import (
	"context"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

func TestWithBoundaries_Roundtrip(t *testing.T) {
	ctx := context.Background()
	b := &paging.Boundaries{
		Values:    []*w17pb.KeysetValue{paging.FromInt64(42)},
		Direction: w17pb.Direction_DIRECTION_FORWARD,
	}
	ctx2 := paging.WithBoundaries(ctx, b)
	got, ok := paging.FromContext(ctx2)
	if !ok {
		t.Fatal("FromContext: want ok=true, got false")
	}
	if got != b {
		t.Errorf("FromContext: pointer identity lost (want %p, got %p)", b, got)
	}
}

func TestFromContext_EmptyContext(t *testing.T) {
	_, ok := paging.FromContext(context.Background())
	if ok {
		t.Fatal("FromContext on empty ctx: want ok=false, got true")
	}
}

func TestWithBoundaries_NilIsNoOp(t *testing.T) {
	ctx := context.Background()
	got := paging.WithBoundaries(ctx, nil)
	if got != ctx {
		t.Errorf("WithBoundaries(nil): want same ctx returned, got different")
	}
	_, ok := paging.FromContext(got)
	if ok {
		t.Fatal("WithBoundaries(nil): FromContext should return ok=false")
	}
}

func TestWithBoundaries_EmptyValuesIsNoOp(t *testing.T) {
	ctx := context.Background()
	got := paging.WithBoundaries(ctx, &paging.Boundaries{
		Values: nil, // explicitly empty
	})
	if got != ctx {
		t.Errorf("WithBoundaries(empty Values): want same ctx returned, got different")
	}
}
