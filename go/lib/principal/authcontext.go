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
