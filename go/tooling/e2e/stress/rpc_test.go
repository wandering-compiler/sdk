package stress

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

const loadProto = `syntax = "proto3";
package ops.v1;
message GetReq { string id = 1; }
message Job { string id = 1; }
`

// An RPC endpoint under load goes over gRPC, spread over the connections,
// and its outcome is bucketed as the HTTP status it stands for.
func TestLoadCaller_RPC(t *testing.T) {
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(protocompile.ResolverFunc(func(p string) (protocompile.SearchResult, error) {
		if p != "ops.proto" {
			return protocompile.SearchResult{}, errors.New("no " + p)
		}
		return protocompile.SearchResult{Source: strings.NewReader(loadProto)}, nil
	}))}
	files, err := c.Compile(context.Background(), "ops.proto")
	if err != nil {
		t.Fatal(err)
	}
	fd := files[0]
	desc, _ := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{protodesc.ToFileDescriptorProto(fd)}})
	reqMD, outMD := fd.Messages().ByName("GetReq"), fd.Messages().ByName("Job")
	var authorized atomic.Int64
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		req := dynamicpb.NewMessage(reqMD)
		if err := stream.RecvMsg(req); err != nil {
			return err
		}
		if md, _ := metadata.FromIncomingContext(stream.Context()); len(md.Get("authorization")) == 1 {
			authorized.Add(1)
		}
		if req.Get(reqMD.Fields().ByName("id")).String() == "nope" {
			return status.Error(codes.NotFound, "no")
		}
		return stream.SendMsg(dynamicpb.NewMessage(outMD))
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	lc, err := NewLoadCaller("http://unused", 4, time.Second).WithRPC(ln.Addr().String(), desc, 3)
	if err != nil {
		t.Fatal(err)
	}
	ep := runner.Endpoint{Ref: "ops.Jobs.Get", Transport: "rpc", AuthRequired: true,
		FullMethod: "/w17lock.ops.rpc.Jobs/Get", RPCRequest: "ops.v1.GetReq", RPCResponse: "ops.v1.Job"}
	for i := range 6 {
		if st, err := lc.FireAs(nil, ep, map[string]any{"id": "j"}, "tok", nil); err != nil || st != 200 {
			t.Fatalf("call %d: %d %v", i, st, err)
		}
	}
	if st, err := lc.FireAs(nil, ep, map[string]any{"id": "nope"}, "tok", nil); err != nil || st != 404 {
		t.Fatalf("%d %v", st, err)
	}
	if authorized.Load() != 7 {
		t.Fatalf("authorized %d", authorized.Load())
	}
	if _, err := NewLoadCaller("http://unused", 1, time.Second).FireAs(nil, ep, nil, "", nil); err == nil {
		t.Fatal("an RPC endpoint fired without RPC connections")
	}
	for code, want := range map[codes.Code]int{codes.OK: 200, codes.InvalidArgument: 400, codes.Unauthenticated: 401,
		codes.PermissionDenied: 403, codes.ResourceExhausted: 429, codes.Unavailable: 503, codes.Internal: 500} {
		if got := rpcStatus(code); got != want {
			t.Errorf("%s → %d, want %d", code, got, want)
		}
	}
}
