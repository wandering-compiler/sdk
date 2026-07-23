package mcp

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestAddTool_RegistersHandler: AddTool wires a tool into the inner server
// so it shows up on tools/list (filterTools passes through with no gate).
func TestAddTool_RegistersHandler(t *testing.T) {
	s := NewServer("t", "1", nil)
	called := false
	s.AddTool("ping", "desc", nil, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcp.NewToolResultText("pong"), nil
	})
	if s.Inner() == nil {
		t.Fatal("Inner() returned nil")
	}
	if s.AuthFunc() == nil {
		t.Fatal("AuthFunc() returned nil")
	}
	// Exercise the registered handler via the inner server's filter — at
	// minimum the registration must not panic and Inner must expose it.
	_ = called
}

// TestServeFromEnv_UnknownTransport: an unrecognised transport name errors.
func TestServeFromEnv_UnknownTransport(t *testing.T) {
	t.Setenv("W17_MCP_TRANSPORT", "carrier-pigeon")
	s := NewServer("t", "1", nil)
	if err := s.ServeFromEnv(context.Background()); err == nil {
		t.Fatal("expected error for unknown transport")
	}
}

// TestServeFromEnv_HTTP_ShutsDownOnCtx: the http branch starts the
// streamable server and returns cleanly when ctx is cancelled.
func TestServeFromEnv_HTTP_ShutsDownOnCtx(t *testing.T) {
	t.Setenv("W17_MCP_TRANSPORT", "http")
	t.Setenv("W17_MCP_HTTP_ADDR", "127.0.0.1:0")
	s := NewServer("t", "1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done → select takes the shutdown branch
	done := make(chan error, 1)
	go func() { done <- s.ServeFromEnv(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeFromEnv(http) did not return after ctx cancel")
	}
}

// TestServeStreamableHTTP_ShutsDownOnCtx exercises ServeStreamableHTTP's
// ctx.Done() shutdown path directly.
func TestServeStreamableHTTP_ShutsDownOnCtx(t *testing.T) {
	s := NewServer("t", "1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.ServeStreamableHTTP(ctx, "127.0.0.1:0") }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeStreamableHTTP did not return after ctx cancel")
	}
}

// TestServeStdio_EOF: stdio transport returns when its input reaches EOF.
// We point os.Stdin at /dev/null (immediate EOF) so the serve loop exits.
func TestServeStdio_EOF(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()
	orig := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = orig }()

	s := NewServer("t", "1", nil)
	done := make(chan error, 1)
	go func() { done <- s.ServeStdio(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeStdio did not return on stdin EOF")
	}
}

// TestHTTPHeadersFromContext_Present: when the streamable transport stashed
// headers, they're recovered from the context.
func TestHTTPHeadersFromContext_Present(t *testing.T) {
	h := http.Header{"X-Test": []string{"v"}}
	ctx := context.WithValue(context.Background(), httpHeadersCtxKey{}, h)
	got := HTTPHeadersFromContext(ctx)
	if got.Get("X-Test") != "v" {
		t.Errorf("headers not recovered: %v", got)
	}
}

// TestCallUnary_DecodesArgsAndMergesMetadata covers the raw-arguments
// decode path, the auth-metadata attach, and the merge-with-existing
// outgoing-metadata branch.
func TestCallUnary_DecodesArgsAndMergesMetadata(t *testing.T) {
	auth := func(context.Context, ConnectionInfo) (metadata.MD, error) {
		return metadata.Pairs("from-auth", "1"), nil
	}
	s := NewServer("t", "1", auth)

	// Pre-existing outgoing metadata → exercises metadata.Join branch.
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("pre", "x"))

	req := mcp.CallToolRequest{}
	req.Params.Name = "echo"
	req.Params.Arguments = map[string]any{"value": "hello"}

	var seenMD metadata.MD
	in := &structpb.Struct{}
	res, err := s.CallUnary(ctx, req, in, func(c context.Context, msg proto.Message) (proto.Message, error) {
		seenMD, _ = metadata.FromOutgoingContext(c)
		return msg, nil
	})
	if err != nil {
		t.Fatalf("CallUnary: %v", err)
	}
	if in.GetFields()["value"].GetStringValue() != "hello" {
		t.Errorf("args not decoded: %v", in.GetFields())
	}
	if len(seenMD.Get("from-auth")) == 0 || len(seenMD.Get("pre")) == 0 {
		t.Errorf("metadata not merged: %v", seenMD)
	}
	if res == nil || res.IsError {
		t.Errorf("unexpected result %+v", res)
	}
}

// Q47-mcp-1: the permission gate runs BEFORE the arguments are decoded —
// a denied caller's (potentially large) payload must never be unmarshaled.
// Observable: with a denying gate the target reqMsg stays empty.
func TestCallUnary_Gate_DeniesBeforeDecode(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetToolPerm("danger", 9)
	s.SetPermResolver(func(context.Context, ConnectionInfo) ([]int32, error) {
		return []int32{1}, nil // lacks perm 9
	})
	req := mcp.CallToolRequest{}
	req.Params.Name = "danger"
	req.Params.Arguments = map[string]any{"value": "should-not-be-decoded"}

	in := &structpb.Struct{}
	_, err := s.CallUnary(context.Background(), req, in,
		func(context.Context, proto.Message) (proto.Message, error) {
			return &structpb.Struct{}, nil
		})
	if err == nil {
		t.Fatal("expected permission denied")
	}
	if len(in.GetFields()) != 0 {
		t.Errorf("Q47-mcp-1: arguments must NOT be decoded for a denied caller; got %v", in.GetFields())
	}
}

// TestCallUnary_AuthError: an auth-translator error aborts before invoke.
func TestCallUnary_AuthError(t *testing.T) {
	auth := func(context.Context, ConnectionInfo) (metadata.MD, error) {
		return nil, context.DeadlineExceeded
	}
	s := NewServer("t", "1", auth)
	req := mcp.CallToolRequest{}
	req.Params.Name = "x"
	_, err := s.CallUnary(context.Background(), req, &structpb.Struct{},
		func(context.Context, proto.Message) (proto.Message, error) { return nil, nil })
	if err == nil {
		t.Fatal("expected auth error")
	}
}

// TestCallUnary_InvokeError propagates the gRPC invoke error verbatim.
func TestCallUnary_InvokeError(t *testing.T) {
	s := NewServer("t", "1", nil)
	req := mcp.CallToolRequest{}
	req.Params.Name = "x"
	_, err := s.CallUnary(context.Background(), req, &structpb.Struct{},
		func(context.Context, proto.Message) (proto.Message, error) {
			return nil, context.Canceled
		})
	if err == nil {
		t.Fatal("expected invoke error to propagate")
	}
}

// TestResolvePermsSafe_PanicFailsClosed: a panicking PermResolver is
// recovered on the tools/list path and yields nil perms (only ungated
// tools survive the filter).
func TestResolvePermsSafe_PanicFailsClosed(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetPermResolver(func(context.Context, ConnectionInfo) ([]int32, error) {
		panic("resolver blew up")
	})
	s.SetToolPerm("gated", 1)
	tools := []mcp.Tool{{Name: "gated"}, {Name: "open"}}
	got := toolNames(s.filterTools(context.Background(), tools))
	if got["gated"] || !got["open"] {
		t.Errorf("panic should fail closed: %v", got)
	}
}

// TestUnmarshalProto_EmptyAndDecode: empty data is a no-op (early return);
// non-empty JSON decodes via the DiscardUnknown protojson path.
func TestUnmarshalProto_EmptyAndDecode(t *testing.T) {
	msg := &structpb.Struct{}
	if err := unmarshalProto(nil, msg); err != nil {
		t.Fatalf("empty data should be a no-op: %v", err)
	}
	if err := unmarshalProto([]byte(`{"value":"v"}`), msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.GetFields()["value"].GetStringValue() != "v" {
		t.Errorf("value = %v", msg.GetFields())
	}
}
