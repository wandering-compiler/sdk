package inprocgrpc_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// The metadata a gateway hands its first backend hop: the principal it
// verified (relayed onward on every hop by design) plus three namespaces
// that are deliberately NOT relayed across a wire hop — tx routing, the
// proxy conn id, and the request language.
//
// `w17-tx-id` is the sharp one: principal.ForwardToOutgoing relays the
// principal and nothing else, and lib/principal/forward_test.go
// (TestForwardToOutgoing_ExcludesNonPrincipal) PINS `w17-tx-id` out of the
// relay — "tx-routing … must stay put". So the wire side of this contract
// is already tested; these tests are its in-process half.
const (
	mdUser     = "x-w17-user"
	mdScopeOrg = "x-w17-scope-org_id"
	mdTxID     = "w17-tx-id"
	mdConnID   = "w17-conn-id"
	mdLanguage = "w17-language"
)

func gatewayMD() metadata.MD {
	return metadata.Pairs(
		mdUser, "envelope-bytes",
		mdScopeOrg, "org-7",
		mdTxID, "tx-abc",
		mdConnID, "conn-9",
		mdLanguage, "cs",
	)
}

// mdRecorder is a stand-in for a generated server impl that records both
// sides of the context it was handed.
type mdRecorder struct {
	incoming   metadata.MD
	outgoing   metadata.MD
	outgoingOK bool
}

func (r *mdRecorder) serve(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	r.incoming, _ = metadata.FromIncomingContext(ctx)
	r.outgoing, r.outgoingOK = metadata.FromOutgoingContext(ctx)
	return wrapperspb.String("ok"), nil
}

// unaryDesc hand-rolls the ServiceDesc a generated RegisterXxxServer would
// pass for one unary method, with the same Handler contract (allocate the
// request, dec it, run the interceptor, call the impl).
func unaryDesc(service, method string, fn func(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)) *grpc.ServiceDesc {
	full := "/" + service + "/" + method
	return &grpc.ServiceDesc{
		ServiceName: service,
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: method,
			Handler: func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.StringValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return fn(ctx, in)
				}
				info := &grpc.UnaryServerInfo{FullMethod: full}
				return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
					return fn(ctx, req.(*wrapperspb.StringValue))
				})
			},
		}},
	}
}

const (
	relayMethod  = "/test.Relay/Relay"
	recordMethod = "/test.Record/Record"
)

// nestedCallFixture wires the shape a composed binary really has: a tier
// reached in-process whose handler makes a FURTHER in-process call with its
// own context (the business facade calling storage through
// NewClientSetInProcess, a plugin's typed-client adapter). `inner` records
// what that nested handler was handed.
func nestedCallFixture(t *testing.T, opts ...any) (*inprocgrpc.Conn, *mdRecorder, *mdRecorder) {
	t.Helper()
	conn := inprocgrpc.New()
	outer := &mdRecorder{}
	inner := &mdRecorder{}

	conn.RegisterService(unaryDesc("test.Record", "Record", inner.serve), nil)
	conn.RegisterService(unaryDesc("test.Relay", "Relay", func(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		if _, err := outer.serve(ctx, in); err != nil {
			return nil, err
		}
		out := new(wrapperspb.StringValue)
		if err := conn.Invoke(ctx, recordMethod, in, out); err != nil {
			return nil, err
		}
		return out, nil
	}), nil)
	return conn, outer, inner
}

func keys(md metadata.MD) []string {
	out := make([]string, 0, len(md))
	for k := range md {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestConn_NestedCall_SeesOnlyWhatAWireHopWouldDeliver pins C-F6. The
// in-process bridge flips the caller's OUTGOING metadata to INCOMING but
// never clears the outgoing side, so a handler reached in-process still
// carries the ORIGINAL caller's outgoing metadata — and any nested
// in-process call it makes re-flips it. `w17-tx-id` therefore rides into
// the nested handler, which ADOPTS the caller's distributed transaction in
// a composed binary while the same code opens a fresh transaction in the
// standalone one: two deployments of one declaration disagreeing about
// transaction boundaries.
//
// The invariant is the wire's: a nested hop receives the principal (the
// dialled-conn interceptor relays it — core/grpcx.DialOpts) and nothing
// else. Stated as an exact key set, so a future namespace that starts
// riding along uninvited fails here rather than in a target project.
func TestConn_NestedCall_SeesOnlyWhatAWireHopWouldDeliver(t *testing.T) {
	conn, _, inner := nestedCallFixture(t)

	ctx := metadata.NewOutgoingContext(context.Background(), gatewayMD())
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(ctx, relayMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	want := []string{mdScopeOrg, mdUser}
	got := keys(inner.incoming)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("nested in-process call saw incoming metadata %v, want %v.\n"+
			"Across a wire hop a nested call carries the relayed principal and NOTHING else "+
			"(lib/principal/forward_test.go pins %s out of the relay); anything extra here is a "+
			"composed-vs-standalone divergence — %s in particular makes the nested handler adopt the "+
			"caller's distributed transaction in a composed binary only.", got, want, mdTxID, mdTxID)
	}
	if v := inner.incoming.Get(mdUser); len(v) != 1 || v[0] != "envelope-bytes" {
		t.Errorf("the principal must still reach the nested handler (composed tiers rely on it and the "+
			"storage scope guard fails closed without it): x-w17-user = %v", v)
	}
}

// TestConn_Handler_StartsWithACleanOutgoingSide is the mechanism behind the
// test above, asserted directly: a real transport builds the server's
// context from the received headers, so a handler's OUTGOING side starts
// empty and everything it forwards downstream is explicit. The in-process
// shortcut must not quietly grant more.
func TestConn_Handler_StartsWithACleanOutgoingSide(t *testing.T) {
	conn, outer, _ := nestedCallFixture(t)

	ctx := metadata.NewOutgoingContext(context.Background(), gatewayMD())
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(ctx, relayMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if len(outer.outgoing) != 0 {
		t.Errorf("handler's OUTGOING metadata = %v, want empty: a wire hop hands the handler a fresh "+
			"outgoing side, so nothing the caller set can ride into a nested call uninvited", keys(outer.outgoing))
	}
	// The incoming side is unchanged — the whole point of the flip.
	if v := outer.incoming.Get(mdTxID); len(v) != 1 || v[0] != "tx-abc" {
		t.Errorf("the DIRECTLY called handler must still receive the caller's full metadata "+
			"(that is the transport contract): w17-tx-id = %v", v)
	}
	if v := outer.incoming.Get(mdLanguage); len(v) != 1 || v[0] != "cs" {
		t.Errorf("first-hop metadata must survive the flip: w17-language = %v", v)
	}
}

// TestConn_CallerWithNoOutgoingMetadata_DoesNotLeakItsIncoming covers the
// second arm. When the caller set no outgoing metadata at all the bridge
// used to skip the flip entirely, leaving its own INCOMING metadata visible
// to the handler — the same divergence from the other side, and the one
// that made composed principal propagation work by accident.
//
// The wire answer: the dialled conn relays the principal out of the
// caller's incoming metadata and the transport delivers exactly that. So
// the principal must arrive (composed tiers depend on it) and nothing else
// may.
func TestConn_CallerWithNoOutgoingMetadata_DoesNotLeakItsIncoming(t *testing.T) {
	conn := inprocgrpc.New()
	rec := &mdRecorder{}
	conn.RegisterService(unaryDesc("test.Record", "Record", rec.serve), nil)

	ctx := metadata.NewIncomingContext(context.Background(), gatewayMD())
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(ctx, recordMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	want := []string{mdScopeOrg, mdUser}
	got := keys(rec.incoming)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("a caller that set NO outgoing metadata delivered %v, want %v: the bridge used to skip "+
			"the flip entirely and leave its own incoming metadata visible, which is how composed "+
			"principal propagation came to work by accident — and how %s came along with it", got, want, mdTxID)
	}
}

// TestConn_UnauthenticatedCaller_StaysUnauthenticated: with no principal
// anywhere, the handler gets an empty incoming side rather than a
// half-inherited one — downstream scope guards must still fail closed.
func TestConn_UnauthenticatedCaller_StaysUnauthenticated(t *testing.T) {
	conn := inprocgrpc.New()
	rec := &mdRecorder{}
	conn.RegisterService(unaryDesc("test.Record", "Record", rec.serve), nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(mdTxID, "tx-abc"))
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(ctx, recordMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(rec.incoming) != 0 {
		t.Errorf("handler saw %v; a caller with no OUTGOING metadata forwards nothing but its principal, "+
			"and there is none here", keys(rec.incoming))
	}
}

// TestConn_Stream_NestedCall_SeesOnlyWhatAWireHopWouldDeliver is the
// streaming twin: NewStream performs the same flip, so it needs the same
// isolation. A composed tier whose streaming handler calls another tier
// would otherwise inherit the caller's outgoing metadata exactly as the
// unary path did.
func TestConn_Stream_NestedCall_SeesOnlyWhatAWireHopWouldDeliver(t *testing.T) {
	conn := inprocgrpc.New()
	inner := &mdRecorder{}
	conn.RegisterService(unaryDesc("test.Record", "Record", inner.serve), nil)

	var handlerIncoming metadata.MD
	var handlerOutgoing metadata.MD
	conn.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName: "Count", ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				ctx := stream.Context()
				handlerIncoming, _ = metadata.FromIncomingContext(ctx)
				handlerOutgoing, _ = metadata.FromOutgoingContext(ctx)
				out := new(wrapperspb.StringValue)
				return conn.Invoke(ctx, recordMethod, wrapperspb.String("x"), out)
			},
		}},
	}, nil)

	ctx := metadata.NewOutgoingContext(context.Background(), gatewayMD())
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	out := new(wrapperspb.StringValue)
	if err := cs.RecvMsg(out); err == nil {
		t.Fatalf("expected the stream to end")
	}

	if v := handlerIncoming.Get(mdTxID); len(v) != 1 {
		t.Errorf("the stream handler itself must still receive the caller's full metadata: w17-tx-id = %v", v)
	}
	if len(handlerOutgoing) != 0 {
		t.Errorf("stream handler's OUTGOING metadata = %v, want empty (same contract as unary)", keys(handlerOutgoing))
	}
	want := []string{mdScopeOrg, mdUser}
	got := keys(inner.incoming)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("call nested inside a STREAMING handler saw %v, want %v", got, want)
	}
}
