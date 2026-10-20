package pluginhook

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeCC answers every call by upper-casing a StringValue and records what
// it was asked.
type fakeCC struct {
	method string
	md     metadata.MD
}

func (f *fakeCC) Invoke(ctx context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	f.method = method
	f.md, _ = metadata.FromOutgoingContext(ctx)
	proto.Merge(reply.(proto.Message), wrapperspb.String(args.(*wrapperspb.StringValue).GetValue()+"!"))
	return nil
}

func (f *fakeCC) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("unused")
}

func TestUnary_LateConn(t *testing.T) {
	c := Late()
	call := Unary[wrapperspb.StringValue, wrapperspb.StringValue](c, "/app.Filters/Clean")
	if _, err := call(context.Background(), wrapperspb.String("x")); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("before Set: %v, want ErrNotConnected", err)
	}
	cc := &fakeCC{}
	c.Set(cc)
	// The verified caller the plugin's handler was called with goes along.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-w17-user", "dXNlcg=="))
	out, err := call(ctx, wrapperspb.String("hi"))
	if err != nil || out.GetValue() != "hi!" || cc.method != "/app.Filters/Clean" {
		t.Fatalf("call = %v, %v (method %q)", out, err, cc.method)
	}
	if got := cc.md.Get("x-w17-user"); len(got) != 1 || got[0] != "dXNlcg==" {
		t.Fatalf("the principal was not relayed: %v", cc.md)
	}
}

func TestUnary_LazyConnDialsOnceAndRetriesAFailure(t *testing.T) {
	dials := 0
	cc := &fakeCC{}
	c := Lazy(func() (grpc.ClientConnInterface, error) {
		dials++
		if dials == 1 {
			return nil, errors.New("not yet")
		}
		return cc, nil
	})
	call := Unary[wrapperspb.StringValue, wrapperspb.StringValue](c, "/app.Filters/Clean")
	if _, err := call(context.Background(), wrapperspb.String("a")); err == nil {
		t.Fatal("a failed dial was not reported")
	}
	for range 2 {
		if _, err := call(context.Background(), wrapperspb.String("a")); err != nil {
			t.Fatal(err)
		}
	}
	if dials != 2 {
		t.Fatalf("dialled %d times, want 2 (one failure, one success, then reused)", dials)
	}
}
