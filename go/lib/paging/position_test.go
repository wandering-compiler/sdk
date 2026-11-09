package paging

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// fakeSTS records what a handler sets on its response headers.
type fakeSTS struct{ header metadata.MD }

func (s *fakeSTS) Method() string { return "/x.S/M" }
func (s *fakeSTS) SetHeader(md metadata.MD) error {
	s.header = metadata.Join(s.header, md)
	return nil
}
func (s *fakeSTS) SendHeader(md metadata.MD) error { return s.SetHeader(md) }
func (s *fakeSTS) SetTrailer(metadata.MD) error    { return nil }

func TestPosition_RoundTrip(t *testing.T) {
	next := []*w17pb.KeysetValue{FromInt64(9)}
	prev := []*w17pb.KeysetValue{FromInt64(4)}
	md := PositionMD(next, prev, 0)
	if got := md.Get(TotalMDKey); len(got) != 1 || got[0] != "0" {
		t.Fatalf("a zero total is a total and must be reported, got %v", got)
	}
	// Next is a FORWARD boundary, Prev a BACKWARD one, in the request side's
	// own encoding.
	if b, ok := DecodeBoundariesMD(md.Get(NextMDKey)[0]); !ok || b.Direction != w17pb.Direction_DIRECTION_FORWARD {
		t.Fatalf("next = %+v", b)
	}
	if b, ok := DecodeBoundariesMD(md.Get(PrevMDKey)[0]); !ok || b.Direction != w17pb.Direction_DIRECTION_BACKWARD {
		t.Fatalf("prev = %+v", b)
	}
	pos, ok := PositionFromHeader(md)
	if !ok || !pos.HasTotal || pos.Total != 0 || !proto.Equal(pos.Next[0], next[0]) || !proto.Equal(pos.Prev[0], prev[0]) {
		t.Fatalf("PositionFromHeader = %+v, %v", pos, ok)
	}
	// An empty page reports only its total.
	pos, ok = PositionFromHeader(PositionMD(nil, nil, 7))
	if !ok || pos.Next != nil || pos.Prev != nil || pos.Total != 7 {
		t.Fatalf("empty page = %+v, %v", pos, ok)
	}
	if _, ok := PositionFromHeader(metadata.MD{}); ok {
		t.Fatal("no position keys must read as no position")
	}
}

func TestSetPosition_WithoutTransportIsANoop(t *testing.T) {
	SetPosition(context.Background(), []*w17pb.KeysetValue{FromInt64(1)}, nil, 1) // must not panic
	sts := &fakeSTS{}
	ctx := grpc.NewContextWithServerTransportStream(context.Background(), sts)
	SetPosition(ctx, []*w17pb.KeysetValue{FromInt64(1)}, nil, 3)
	if _, ok := PositionFromHeader(sts.header); !ok {
		t.Fatalf("SetPosition set %v", sts.header)
	}
}

// Only a call made with Forward relays: its response's position lands on
// the business method's own response; a sibling call's page does not.
func TestRelayPositionUnaryInterceptor(t *testing.T) {
	answered := PositionMD([]*w17pb.KeysetValue{FromInt64(2)}, []*w17pb.KeysetValue{FromInt64(1)}, 5)
	answered.Set("x-other", "not relayed")
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		for _, o := range opts {
			if h, ok := o.(grpc.HeaderCallOption); ok {
				*h.HeaderAddr = answered
			}
		}
		return nil
	}
	incoming := metadata.Pairs(LimitMDKey, "2")

	sts := &fakeSTS{}
	base := grpc.NewContextWithServerTransportStream(metadata.NewIncomingContext(context.Background(), incoming), sts)
	if err := RelayPositionUnaryInterceptor(base, "/x.Q/List", nil, nil, nil, invoker); err != nil {
		t.Fatal(err)
	}
	if sts.header != nil {
		t.Fatalf("a call not made with Forward relayed %v", sts.header)
	}
	if err := RelayPositionUnaryInterceptor(Forward(base), "/x.Q/List", nil, nil, nil, invoker); err != nil {
		t.Fatal(err)
	}
	pos, ok := PositionFromHeader(sts.header)
	if !ok || pos.Total != 5 || len(pos.Next) != 1 || len(sts.header.Get("x-other")) != 0 {
		t.Fatalf("relayed %v", sts.header)
	}
	if Forwarded(WithoutForward(Forward(base))) {
		t.Fatal("WithoutForward must drop the mark")
	}
}
