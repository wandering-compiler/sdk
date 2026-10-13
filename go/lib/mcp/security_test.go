package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/core/healthz"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// ---- identity: env credentials are stdio-only ----

// An HTTP caller that sends no credential must not inherit the stdio
// identity from the environment, and no `<PREFIX>_*` variable may be
// forwarded on its behalf.
func TestConnInfo_HTTPCallerGetsNoEnvIdentity(t *testing.T) {
	t.Setenv("ZZAPP_AUTHORIZATION", "Bearer operator-token")
	t.Setenv("ZZAPP_DATABASE_URL", "postgres://secret")
	s := NewServer("x", "1", nil)
	s.SetAuthEnvPrefix("ZZAPP")

	httpCtx := context.WithValue(context.Background(), httpHeadersCtxKey{}, http.Header{"User-Agent": {"curl"}})
	got := CollectAuthHeaders(s.connInfo(httpCtx))
	if v, ok := got["authorization"]; ok {
		t.Errorf("HTTP caller without a credential got authorization=%q from env", v)
	}
	if v, ok := got["database-url"]; ok {
		t.Errorf("env variable forwarded as a header on HTTP: database-url=%q", v)
	}
	if got["user-agent"] != "curl" {
		t.Errorf("request headers must still be collected, got %v", got)
	}

	// A nil header map stashed by a transport is still an HTTP connection.
	nilCtx := context.WithValue(context.Background(), httpHeadersCtxKey{}, http.Header(nil))
	if v, ok := CollectAuthHeaders(s.connInfo(nilCtx))["authorization"]; ok {
		t.Errorf("HTTP caller (nil header map) got authorization=%q from env", v)
	}

	// stdio keeps the env identity.
	if got := CollectAuthHeaders(s.connInfo(context.Background())); got["authorization"] != "Bearer operator-token" {
		t.Errorf("stdio must source the credential from env, got %v", got)
	}
}

// DefaultAuthFunc's process-environment fallback is a stdio feature.
func TestDefaultAuthFunc_HTTPHasNoEnvFallback(t *testing.T) {
	t.Setenv("AUTHORIZATION", "Bearer operator-token")
	fn := DefaultAuthFunc("authorization")

	md, err := fn(context.Background(), ConnectionInfo{
		HTTPHeaders: http.Header{},
		Env:         map[string]string{"AUTHORIZATION": "Bearer env-map"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := md.Get("authorization"); len(v) != 0 {
		t.Errorf("HTTP caller without a credential got authorization=%v", v)
	}

	md, _ = fn(context.Background(), ConnectionInfo{})
	if v := md.Get("authorization"); len(v) != 1 || v[0] != "Bearer operator-token" {
		t.Errorf("stdio must keep the env fallback, got %v", v)
	}
}

// End to end over the real HTTP transport: the auth translator sees only
// what the request carried.
func TestServeStreamableHTTP_AnonymousCallerGetsNoEnvIdentity(t *testing.T) {
	t.Setenv("ZZAPP_AUTHORIZATION", "Bearer operator-token")
	t.Setenv("ZZAPP_SECRET_KEY", "k")

	var mu sync.Mutex
	var seen map[string]string
	auth := func(_ context.Context, c ConnectionInfo) (metadata.MD, error) {
		mu.Lock()
		seen = CollectAuthHeaders(c)
		mu.Unlock()
		return DefaultAuthFunc("authorization")(context.Background(), c)
	}
	s := NewServer("x", "1", auth)
	s.SetAuthEnvPrefix("ZZAPP")
	var sentMD metadata.MD
	s.AddTool("t", "d", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return s.CallUnary(ctx, req, &emptypb.Empty{}, func(ctx context.Context, m proto.Message) (proto.Message, error) {
			mu.Lock()
			sentMD, _ = metadata.FromOutgoingContext(ctx)
			mu.Unlock()
			return &emptypb.Empty{}, nil
		})
	})

	base := startHTTP(t, s)
	resp := callToolHTTP(t, base, "t")
	if !strings.Contains(resp, `"result"`) {
		t.Fatalf("tool call failed: %s", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if v, ok := seen["authorization"]; ok {
		t.Errorf("anonymous HTTP caller authenticated with env credential %q", v)
	}
	if v, ok := seen["secret-key"]; ok {
		t.Errorf("env variable forwarded as header on HTTP: secret-key=%q", v)
	}
	if v := sentMD.Get("authorization"); len(v) != 0 {
		t.Errorf("backend metadata carries env credential %v", v)
	}
}

// ---- tool errors: same public mapping as REST ----

const leakyDev = "UserMutation.CreateUser: unmapped unique constraint users_email_key"

func leakyStatus(t *testing.T) error {
	t.Helper()
	st, err := status.New(codes.FailedPrecondition, leakyDev).WithDetails(
		&w17pb.ErrorDetail{Field: "email", Code: "UNIQUE_VIOLATION", Message: "This value is already taken."})
	if err != nil {
		t.Fatal(err)
	}
	return st.Err()
}

func TestCallUnary_BackendErrorIsSanitised(t *testing.T) {
	s := NewServer("x", "1", nil)
	backendErr := leakyStatus(t)
	s.AddTool("t", "d", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return s.CallUnary(ctx, req, &emptypb.Empty{}, func(context.Context, proto.Message) (proto.Message, error) {
			return nil, backendErr
		})
	})
	resp := s.Inner().HandleMessage(context.Background(),
		json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{}}}`))
	b, _ := json.Marshal(resp)
	wire := string(b)
	for _, leak := range []string{"users_email_key", "UserMutation", "unmapped"} {
		if strings.Contains(wire, leak) {
			t.Errorf("MCP response leaks %q: %s", leak, wire)
		}
	}

	// The client reads what a REST client reads for the same failure.
	c, msg, details := restgw.ClientError(context.Background(), backendErr)
	if !strings.Contains(wire, "code = "+c.String()) || !strings.Contains(wire, msg) {
		t.Errorf("MCP response must carry code %s and sentence %q: %s", c, msg, wire)
	}

	// The returned status keeps the field details.
	_, err := s.CallUnary(context.Background(), mcp.CallToolRequest{}, &emptypb.Empty{},
		func(context.Context, proto.Message) (proto.Message, error) { return nil, backendErr })
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition || st.Message() != msg {
		t.Errorf("status = %v %q, want %v %q", st.Code(), st.Message(), c, msg)
	}
	var got []*w17pb.ErrorDetail
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			got = append(got, ed)
		}
	}
	if len(got) != len(details) || len(got) != 1 || got[0].GetField() != "email" || got[0].GetCode() != "UNIQUE_VIOLATION" {
		t.Errorf("details not carried: %v", got)
	}
}

func TestCallUnary_AuthBackendErrorIsSanitised(t *testing.T) {
	auth := func(context.Context, ConnectionInfo) (metadata.MD, error) {
		return nil, status.Error(codes.Unauthenticated, "auth_token row 42 revoked (AuthService.Authenticate)")
	}
	s := NewServer("x", "1", auth)
	_, err := s.CallUnary(context.Background(), mcp.CallToolRequest{}, &emptypb.Empty{},
		func(context.Context, proto.Message) (proto.Message, error) { return &emptypb.Empty{}, nil })
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated (%v)", status.Code(err), err)
	}
	if strings.Contains(err.Error(), "auth_token") || strings.Contains(err.Error(), "AuthService") {
		t.Errorf("auth backend text leaks: %v", err)
	}
}

func TestCallUnary_NonStatusErrorIsInternal(t *testing.T) {
	s := NewServer("x", "1", nil)
	_, err := s.CallUnary(context.Background(), mcp.CallToolRequest{}, &emptypb.Empty{},
		func(context.Context, proto.Message) (proto.Message, error) {
			return nil, errors.New("dial tcp 10.0.0.5:50051: connection refused")
		})
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "10.0.0.5") {
		t.Errorf("want INTERNAL without transport detail, got %v", err)
	}
}

func TestCallUnary_PermissionDeniedIsAStatus(t *testing.T) {
	s := NewServer("x", "1", nil)
	s.SetToolPerm("danger", 9)
	s.SetPermResolver(func(context.Context, ConnectionInfo) ([]int32, error) { return nil, nil })
	req := mcp.CallToolRequest{}
	req.Params.Name = "danger"
	_, err := s.CallUnary(context.Background(), req, &emptypb.Empty{},
		func(context.Context, proto.Message) (proto.Message, error) { return &emptypb.Empty{}, nil })
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("want PERMISSION_DENIED, got %v", err)
	}
}

// ---- request body cap ----

func TestLimitBody(t *testing.T) {
	var read int
	var readErr error
	h := limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		read, readErr = len(b), err
	}), 16)

	// Declared length over the cap: refused before the handler runs.
	read, readErr = -1, nil
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("x", 17))))
	if rec.Code != http.StatusRequestEntityTooLarge || read != -1 {
		t.Errorf("declared oversize: status %d, handler read %d", rec.Code, read)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("413 must carry a JSON-RPC error, got %s", rec.Body.String())
	}

	// No declared length (chunked): the read stops at the cap.
	req := httptest.NewRequest(http.MethodPost, "/mcp", io.MultiReader(strings.NewReader(strings.Repeat("x", 1<<20))))
	req.ContentLength = -1
	h.ServeHTTP(httptest.NewRecorder(), req)
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) || read > 16 {
		t.Errorf("chunked oversize: read %d bytes, err %v", read, readErr)
	}

	// Within the cap: untouched.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("small")))
	if read != 5 || readErr != nil {
		t.Errorf("small body: read %d, err %v", read, readErr)
	}
}

func TestMaxBodyBytes_DefaultAndOverrides(t *testing.T) {
	s := NewServer("x", "1", nil)
	if got := s.maxBodyBytes(); got != DefaultMaxRequestBodyBytes {
		t.Errorf("default = %d", got)
	}
	t.Setenv(MaxBodyBytesEnv, "2048")
	if got := s.maxBodyBytes(); got != 2048 {
		t.Errorf("env override = %d", got)
	}
	s.SetMaxRequestBodyBytes(512)
	if got := s.maxBodyBytes(); got != 512 {
		t.Errorf("explicit = %d", got)
	}
}

// End to end: the HTTP transport refuses an oversized POST.
func TestServeStreamableHTTP_RefusesOversizedBody(t *testing.T) {
	s := NewServer("x", "1", nil)
	s.SetMaxRequestBodyBytes(1024)
	base := startHTTP(t, s)

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"pad":"` + strings.Repeat("x", 4096) + `"}}`
	req, _ := http.NewRequest(http.MethodPost, base+EndpointPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 413: %s", resp.StatusCode, b)
	}
}

// ---- helpers ----

func startHTTP(t *testing.T, s *Server) string {
	t.Helper()
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.ServeStreamableHTTP(ctx, addr); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	})
	base := "http://" + addr
	for i := 0; i < 200; i++ {
		if resp, err := http.Get(base + healthz.Path); err == nil {
			_ = resp.Body.Close()
			return base
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("MCP HTTP transport did not come up")
	return ""
}

func postMCP(t *testing.T, url, session, body string, extra ...http.Header) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	for _, h := range extra {
		for k, vs := range h {
			req.Header[k] = vs
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func callToolHTTP(t *testing.T, base, name string, extra ...http.Header) string {
	t.Helper()
	url := base + EndpointPath
	resp, body := postMCP(t, url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, extra...)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: %d %s", resp.StatusCode, body)
	}
	session := resp.Header.Get("Mcp-Session-Id")
	_, _ = postMCP(t, url, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, extra...)
	_, body = postMCP(t, url, session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+name+`","arguments":{}}}`, extra...)
	return body
}
