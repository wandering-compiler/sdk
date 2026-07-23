package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/protojsonx"
)

// PermResolver resolves the calling connection's effective permission
// IDs — typically by calling the backend AuthService.Authenticate with
// the connection's bearer / API token. nil = no gating: the MCP surface
// is open, every tool is listed + callable (today's behaviour). The
// generated main injects one via SetPermResolver only when the surface
// carries API-token auth.
type PermResolver func(ctx context.Context, conn ConnectionInfo) (perms []int32, err error)

// Server wraps an mcp-go server and centralises the
// auth/metadata pipeline so generated handler files only
// have to register tools.
type Server struct {
	inner *mcpserver.MCPServer
	auth  MCPAuthFunc

	// permResolver + toolPerms drive permission gating. Both empty/nil
	// => no gate (every tool open). When set, a connection sees + can
	// call only the tools whose permission id its token covers.
	permResolver PermResolver
	toolPerms    map[string][]int32

	// authEnv is a prefix-scoped snapshot of the process environment used
	// as the stdio identity source (Q63-mcp-1): on stdio there are no HTTP
	// headers, so the auth/permission resolvers source the connection's
	// token from `<prefix>_<HEADER>` env vars instead. nil until
	// SetAuthEnvPrefix is called (default: no env forwarding). Captured
	// once (the process environment is fixed for a server's lifetime),
	// SCREAMING_SNAKE-keyed with the prefix stripped, matching envKey so
	// CollectAuthHeaders + lookup read it consistently.
	authEnv map[string]string
}

// NewServer constructs a Server with the given name/version
// and auth translator. Passing nil auth is equivalent to a
// no-op translator (no metadata attached) — useful for
// servers that don't need per-call identity.
func NewServer(name, version string, auth MCPAuthFunc) *Server {
	if auth == nil {
		auth = func(_ context.Context, _ ConnectionInfo) (metadata.MD, error) {
			return metadata.MD{}, nil
		}
	}
	s := &Server{
		auth:      auth,
		toolPerms: map[string][]int32{},
	}
	// WithToolFilter must be set at construction (mcp-go option). The
	// filter is a no-op until a PermResolver is injected.
	s.inner = mcpserver.NewMCPServer(name, version,
		mcpserver.WithToolFilter(s.filterTools),
	)
	return s
}

// SetAuthEnvPrefix enables stdio identity forwarding (Q63-mcp-1): it
// snapshots the process environment, keeping only `<prefix>_<KEY>` vars
// (prefix + the joining `_` stripped, so `MYAPP_AUTHORIZATION` becomes
// `AUTHORIZATION`). Those land on every ConnectionInfo's Env, so the auth
// translator + permission resolver can source the connection's token from
// env when the HTTP-header channel is absent (stdio). Scoping to the
// prefix bounds exposure to the project's own namespace — the whole
// environment is never forwarded. Empty prefix is a no-op (env forwarding
// stays off). Generated main calls this with the project's ENV prefix when
// the MCP surface carries auth.
func (s *Server) SetAuthEnvPrefix(prefix string) {
	if prefix == "" {
		return
	}
	want := prefix + "_"
	env := map[string]string{}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k, v := kv[:eq], kv[eq+1:]
		if rest, ok := strings.CutPrefix(k, want); ok && rest != "" {
			env[rest] = v
		}
	}
	if len(env) > 0 {
		s.authEnv = env
	}
}

// connInfo builds the ConnectionInfo for the current request, threading
// the prefix-scoped env snapshot alongside any HTTP headers so the auth
// resolvers see one identity view across transports.
func (s *Server) connInfo(ctx context.Context) ConnectionInfo {
	return ConnectionInfo{
		HTTPHeaders: HTTPHeadersFromContext(ctx),
		Env:         s.authEnv,
	}
}

// SetPermResolver turns permission gating ON: the resolver is consulted
// on tools/list (to filter) + tools/call (to authorize). Leaving it
// unset keeps the surface open. Generated main calls this only when the
// MCP surface declares an auth method.
func (s *Server) SetPermResolver(r PermResolver) { s.permResolver = r }

// SetToolPerm records the permission id a tool requires. A tool with no
// recorded perm (or perm 0) is ungated — always listed + callable.
// Generated Register<Svc>Tools calls this alongside AddTool when the
// method carries an MCP perm.
//
// Single-id convenience wrapper over SetToolPerms.
func (s *Server) SetToolPerm(name string, permID int32) {
	s.SetToolPerms(name, permID)
}

// SetToolPerms records EVERY permission id a tool requires; the caller
// must cover ALL of them (AND, not OR) to list or call it. Zero ids are
// dropped (0 = "no perm"), and a tool left with no ids stays ungated.
//
// A tool needs more than one id because the ACL cascade gates a method
// on both its endpoint perm and — for model-backed methods — the model
// perm (`<module>.<Model>#view`). Enforcing only the endpoint perm let a
// token carrying it read the model through MCP while REST answered 403;
// the same transport-parity argument is recorded for the RPC gateway in
// docs/decisions/rpc-transport-enforces-acl.md (Q57-gateway-1).
func (s *Server) SetToolPerms(name string, permIDs ...int32) {
	ids := make([]int32, 0, len(permIDs))
	for _, id := range permIDs {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		s.toolPerms[name] = ids
	}
}

// AddTool registers an MCP tool. The handler should perform
// the gRPC call and return a CallToolResult; use CallUnary
// to build a standard one.
func (s *Server) AddTool(name, description string, rawInputSchema json.RawMessage, handler mcpserver.ToolHandlerFunc) {
	tool := mcp.Tool{
		Name:           name,
		Description:    description,
		RawInputSchema: rawInputSchema,
	}
	s.inner.AddTool(tool, recoverTool(name, handler))
}

// recoverTool wraps a tool handler so a panic in tool business logic
// returns an MCP error result instead of relying on the vendored
// mark3labs/mcp-go to recover it — if it doesn't, one bad tool call
// would crash the whole gateway process. The panic value + stack go to
// observx (Sentry + OTel; always "not noise"); the client gets a
// generic error result and the connection stays up.
func recoverTool(name string, h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				observx.ReportError(ctx, fmt.Errorf("PANIC MCP tool %q: %v\n%s", name, r, debug.Stack()))
				res = mcp.NewToolResultError("internal error")
				err = nil
			}
		}()
		return h(ctx, req)
	}
}

// filterTools implements the dynamic tools/list: a connection sees only
// the tools its token's permissions cover. No-op (every tool) when
// gating is off. Fail-closed — an auth error (or absent token) hides
// every GATED tool; ungated tools still show.
func (s *Server) filterTools(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	if s.permResolver == nil {
		return tools
	}
	perms := s.resolvePermsSafe(ctx)
	out := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		permIDs, gated := s.toolPerms[t.Name]
		if !gated || coversAllInt32(perms, permIDs) {
			out = append(out, t)
		}
	}
	return out
}

// resolvePermsSafe runs the permResolver for the tools/list path,
// treating BOTH an error AND a panic as fail-closed (nil perms → only
// ungated tools survive). tools/list runs on the mcp-go request
// goroutine, which — unlike tools/call (recoverTool) — is not otherwise
// wrapped; without this a permResolver panic could crash the gateway.
func (s *Server) resolvePermsSafe(ctx context.Context) (perms []int32) {
	defer func() {
		if r := recover(); r != nil {
			observx.ReportError(ctx, fmt.Errorf("PANIC MCP permResolver (tools/list): %v\n%s", r, debug.Stack()))
			perms = nil // fail closed
		}
	}()
	p, err := s.permResolver(ctx, s.connInfo(ctx))
	if err != nil {
		return nil // fail closed: only ungated tools survive
	}
	return p
}

func containsInt32(s []int32, v int32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// coversAllInt32 reports whether held covers EVERY id in required. A
// tool's ids are ANDed: the endpoint perm alone must not unlock a
// method whose model perm the caller lacks.
func coversAllInt32(held, required []int32) bool {
	for _, id := range required {
		if !containsInt32(held, id) {
			return false
		}
	}
	return true
}

// Inner exposes the underlying mcp-go server for advanced
// use cases (custom middlewares, prompts, resources).
// Generated code should not need it.
func (s *Server) Inner() *mcpserver.MCPServer { return s.inner }

// AuthFunc returns the configured MCPAuthFunc. Generated
// dispatchers use it to build gRPC metadata for each tool
// call.
func (s *Server) AuthFunc() MCPAuthFunc { return s.auth }

// ServeStdio runs the server over stdio (newline-delimited
// JSON-RPC). This is the transport used by Claude Desktop
// and most local MCP clients. The provided ctx is propagated
// into every per-request context.
func (s *Server) ServeStdio(ctx context.Context) error {
	return mcpserver.ServeStdio(s.inner, mcpserver.WithStdioContextFunc(func(_ context.Context) context.Context {
		return ctx
	}))
}

// ServeStreamableHTTP runs the server over the streamable
// HTTP transport at the given address. Used for remote MCP
// clients and proxies behind reverse proxies.
//
// Per-request HTTP headers are stashed on the request
// context via WithHTTPContextFunc so the auth translator
// can read them through HTTPHeadersFromContext.
func (s *Server) ServeStreamableHTTP(ctx context.Context, addr string) error {
	srv := mcpserver.NewStreamableHTTPServer(s.inner,
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, httpHeadersCtxKey{}, r.Header)
		}),
	)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(addr) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// ServeFromEnv selects the transport based on
// W17_MCP_TRANSPORT (`stdio` or `http`, default `stdio`)
// and W17_MCP_HTTP_ADDR (default `:8081`).
func (s *Server) ServeFromEnv(ctx context.Context) error {
	switch transport := strings.ToLower(os.Getenv("W17_MCP_TRANSPORT")); transport {
	case "", "stdio":
		return s.ServeStdio(ctx)
	case "http", "streamable", "streamable_http":
		addr := os.Getenv("W17_MCP_HTTP_ADDR")
		if addr == "" {
			addr = ":8081"
		}
		return s.ServeStreamableHTTP(ctx, addr)
	default:
		return fmt.Errorf("unknown W17_MCP_TRANSPORT %q (want: stdio | http)", transport)
	}
}

// HTTPHeadersFromContext extracts HTTP headers from the
// incoming MCP request when the transport is streamable
// HTTP. Returns nil for stdio (no inbound HTTP request) or
// when the value was not stashed.
func HTTPHeadersFromContext(ctx context.Context) http.Header {
	if v, ok := ctx.Value(httpHeadersCtxKey{}).(http.Header); ok {
		return v
	}
	return nil
}

type httpHeadersCtxKey struct{}

// CallUnary is the generic dispatcher used by every generated
// tool handler. It decodes the MCP arguments JSON into reqMsg,
// runs the auth translator, attaches metadata to the outgoing
// context, calls invoke (which performs the typed gRPC call),
// then marshals the response back into a text-content
// CallToolResult.
//
// invoke takes the prepared context and the populated request
// message and returns the response message — never copy a
// proto.Message by value, so we never write into a caller-
// supplied respMsg.
func (s *Server) CallUnary(
	ctx context.Context,
	req mcp.CallToolRequest,
	reqMsg proto.Message,
	invoke func(ctx context.Context, reqMsg proto.Message) (proto.Message, error),
) (*mcp.CallToolResult, error) {
	conn := s.connInfo(ctx)

	// Authorization runs BEFORE decoding the arguments (Q47-mcp-1): an
	// unauthorized caller is refused without the server spending CPU
	// unmarshaling a (potentially large) payload it has no right to submit,
	// and the generated handler defers the backend dial until after this
	// returns (Q47-mcp-2) so a denied call never touches the upstream.

	// Permission gate — when gating is on and this tool requires a perm,
	// the connection's token must cover it. The dynamic tools/list
	// already hides what the token can't reach; this is the enforcement
	// half (a client that calls a tool by name anyway is refused).
	// Ungated tools + open surfaces (no PermResolver) skip it.
	if s.permResolver != nil {
		if permIDs, gated := s.toolPerms[req.Params.Name]; gated {
			perms, err := s.permResolver(ctx, conn)
			if err != nil {
				return nil, fmt.Errorf("mcp auth: %w", err)
			}
			if !coversAllInt32(perms, permIDs) {
				return nil, fmt.Errorf("mcp: permission denied for tool %q", req.Params.Name)
			}
		}
	}

	md, err := s.auth(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("mcp auth: %w", err)
	}
	if len(md) > 0 {
		// Merge with any pre-existing outgoing metadata
		// instead of replacing — middlewares or callers may
		// have already attached their own.
		if existing, ok := metadata.FromOutgoingContext(ctx); ok {
			md = metadata.Join(existing, md)
		}
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	// Decode only after authz passes.
	if raw := req.GetRawArguments(); raw != nil {
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("encode arguments: %w", err)
		}
		if err := unmarshalProto(data, reqMsg); err != nil {
			return nil, fmt.Errorf("decode arguments into %T: %w", reqMsg, err)
		}
	}

	respMsg, err := invoke(ctx, reqMsg)
	if err != nil {
		return nil, err
	}

	out, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(respMsg)
	if err != nil {
		return nil, fmt.Errorf("marshal response: %w", err)
	}
	// Route through the w17 JSON dialect so an MCP tool result carries the
	// FE-friendly collapsed oneof — matching the tool's registered schema
	// (gateway/schema emits the discriminator) and every other JSON
	// surface. No alias rewrite on MCP, so the resolver is nil.
	if desc := respMsg.ProtoReflect().Descriptor(); protojsonx.HasAnyOneof(desc) {
		out, err = protojsonx.CollapseOneofs(out, desc, nil)
		if err != nil {
			return nil, fmt.Errorf("collapse oneof: %w", err)
		}
	}
	return mcp.NewToolResultText(string(out)), nil
}

// unmarshalProto wraps protojson with the gateway's standard
// "discard unknown fields" stance so MCP clients adding
// optional fields don't break tool calls.
func unmarshalProto(data []byte, msg proto.Message) error {
	if len(data) == 0 {
		return nil
	}
	// Expand the collapsed-oneof dialect (the shape the tool's input
	// schema advertises) back to protojson's flat form before decoding.
	if msg != nil {
		if desc := msg.ProtoReflect().Descriptor(); protojsonx.HasAnyOneof(desc) {
			if expanded, err := protojsonx.ExpandOneofs(data, desc, nil); err == nil {
				data = expanded
			}
		}
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, msg)
}
