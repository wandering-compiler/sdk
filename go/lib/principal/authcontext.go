package principal

// The request context a gateway hands its auth method beside the client's
// own headers, under keys it owns: client copies are stripped at every edge
// (they are `x-w17-*`, so [IsEdgeReservedKey] refuses them), so what the auth
// method reads under these names the gateway wrote.
const (
	// ClientAddrKey is the client's address: the immediate peer, or — when
	// the peer is a configured trusted proxy — the right-most
	// X-Forwarded-For hop that is not itself one. Empty when the surface
	// has no peer (the CLI, MCP over stdio).
	ClientAddrKey = "x-w17-client-addr"
	// SurfaceKey names the surface the request came in on: one of the
	// Surface* values.
	SurfaceKey = "x-w17-surface"
	// MethodKey is what was called: the full gRPC method
	// (`/<package>.<Service>/<Method>`) wherever one backs the call; on the
	// event stream `GET <route>`, on the admin surface `<HTTP method>
	// <path>`; empty on the CLI.
	MethodKey = "x-w17-method"
	// MCPResourceKey is the OAuth resource (RFC 8707) an MCP surface that is
	// a protected resource serves: its public URL. The auth method refuses a
	// token issued for another resource. Absent on every other surface.
	MCPResourceKey = "x-w17-mcp-resource"
	// AudienceKey is the audience the gateway serves, when its domain
	// publishes several (w17/audience.proto); absent on a gateway without
	// one.
	AudienceKey = "x-w17-audience"
	// MembershipKindsKey is the comma-separated membership kinds that
	// audience accepts. The auth method admits a caller only through a
	// membership of one of them, and resolves the tenant among those.
	// Absent when the audience accepts any.
	MembershipKindsKey = "x-w17-membership-kinds"
)

// The SurfaceKey values.
const (
	SurfaceREST   = "rest"
	SurfaceEvents = "events"
	SurfaceRPC    = "rpc"
	SurfaceMCP    = "mcp"
	SurfaceAdmin  = "admin"
	SurfaceCLI    = "cli"
)
