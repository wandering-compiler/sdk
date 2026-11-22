package runner

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

const rpcTestProto = `syntax = "proto3";
package w17lock.shop.rpc;
message GetOrderReq { string id = 1; int32 qty = 2; }
message Order { string id = 1; string customer_name = 2; Kind kind = 3; repeated string tags = 4;
  enum Kind { KIND_UNSPECIFIED = 0; KIND_RETAIL = 1; } }
service ShopRpc { rpc GetOrder(GetOrderReq) returns (Order); }
`

// rpcServer serves ShopRpc dynamically: an order echoing the request, the
// caller's authorization in customer_name, NOT_FOUND for id "missing".
func rpcServer(t *testing.T) (addr string, descriptors []byte) {
	t.Helper()
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(protocompile.ResolverFunc(func(p string) (protocompile.SearchResult, error) {
		if p != "shop.proto" {
			return protocompile.SearchResult{}, errors.New("no " + p)
		}
		return protocompile.SearchResult{Source: strings.NewReader(rpcTestProto)}, nil
	}))}
	files, err := c.Compile(context.Background(), "shop.proto")
	if err != nil {
		t.Fatal(err)
	}
	fd := files[0]
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{protodesc.ToFileDescriptorProto(fd)}}
	descriptors, _ = proto.Marshal(set)
	reqMD, outMD := fd.Messages().ByName("GetOrderReq"), fd.Messages().ByName("Order")

	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		m, _ := grpc.MethodFromServerStream(stream)
		if m != "/w17lock.shop.rpc.ShopRpc/GetOrder" {
			return status.Error(codes.Unimplemented, m)
		}
		req := dynamicpb.NewMessage(reqMD)
		if err := stream.RecvMsg(req); err != nil {
			return err
		}
		id := req.Get(reqMD.Fields().ByName("id")).String()
		if id == "missing" {
			st, _ := status.New(codes.NotFound, "no such order").WithDetails(&w17pb.ErrorDetail{Field: "id", Code: "not_found", Message: "no order " + id})
			return st.Err()
		}
		md, _ := metadata.FromIncomingContext(stream.Context())
		out := dynamicpb.NewMessage(outMD)
		f := outMD.Fields()
		out.Set(f.ByName("id"), protoreflect.ValueOfString(id))
		out.Set(f.ByName("customer_name"), protoreflect.ValueOfString(strings.Join(md.Get("authorization"), ",")+"|"+strings.Join(md.Get("w17-org"), ",")))
		out.Set(f.ByName("kind"), protoreflect.ValueOfEnum(1))
		return stream.SendMsg(out)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String(), descriptors
}

var getOrder = Endpoint{Ref: "shop.Shop.GetOrder", Transport: "rpc", AuthRequired: true,
	FullMethod: "/w17lock.shop.rpc.ShopRpc/GetOrder", RPCRequest: "w17lock.shop.rpc.GetOrderReq", RPCResponse: "w17lock.shop.rpc.Order"}

func TestRPCCaller(t *testing.T) {
	addr, desc := rpcServer(t)
	c, err := NewRPCCaller(addr, desc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	got, err := c.Call(ctx, getOrder, map[string]any{"id": "o-1", "qty": 2}, "tok", map[string]string{"W17-Org": "acme"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The shape REST writes: proto names, enums as numbers, defaults left out.
	if got["id"] != "o-1" || got["customer_name"] != "Bearer tok|acme" || got["kind"] != float64(1) {
		t.Fatalf("%v", got)
	}
	if _, ok := got["tags"]; ok {
		t.Fatalf("an empty field was written: %v", got)
	}

	// A credential replaces the bearer; an endpoint without auth sends none.
	got, _ = c.Call(ctx, getOrder, map[string]any{"id": "o-1"}, "tok", nil, "Basic eA==", nil)
	if got["customer_name"] != "Basic eA==|" {
		t.Fatalf("%v", got)
	}
	open := getOrder
	open.AuthRequired = false
	if got, _ = c.Call(ctx, open, map[string]any{"id": "o-1"}, "tok", nil, "", nil); got["customer_name"] != "|" {
		t.Fatalf("%v", got)
	}

	// A refusal: the code in the shared spelling, the details kept.
	_, err = c.Call(ctx, getOrder, map[string]any{"id": "missing"}, "tok", nil, "", nil)
	var ce *CallError
	if !errors.As(err, &ce) || ce.Code != "NOT_FOUND" || ce.Message != "no such order" || len(ce.Details) != 1 || ce.Details[0].Field != "id" {
		t.Fatalf("%v", err)
	}

	// Input that does not fit the request message, and files, are the
	// step's mistakes, said as such.
	if _, err := c.Call(ctx, getOrder, map[string]any{"nope": 1}, "tok", nil, "", nil); err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("%v", err)
	}
	if _, err := c.Call(ctx, getOrder, nil, "tok", nil, "", []FilePart{{}}); err == nil || !strings.Contains(err.Error(), "multipart") {
		t.Fatalf("%v", err)
	}
	bad := getOrder
	bad.RPCRequest = "w17lock.shop.rpc.Nope"
	if _, err := c.Call(ctx, bad, nil, "tok", nil, "", nil); err == nil || !strings.Contains(err.Error(), "not in the baked descriptors") {
		t.Fatalf("%v", err)
	}
}
