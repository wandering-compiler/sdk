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
	// TenantFromKey is the comma-separated, ordered places the audience
	// takes a request's tenant from: the TenantFrom* values. Absent when the
	// audience names none, which is the selector or the sole membership.
	TenantFromKey = "x-w17-tenant-from"
	// HostKey is the host the request arrived on, lower-case and without a
	// port: the Host header, or X-Forwarded-Host when the peer is a trusted
	// proxy. Written on REST; absent where a transport has no host.
	HostKey = "x-w17-host"
	// ProductKey names the product domain whose gateway asks — set by a
	// product's gateway authenticating against its identity domain
	// ((w17.domain).identity). The identity answers the role keys the caller
	// holds in that product.
	ProductKey = "x-w17-product"
)

// The TenantFromKey values, one per w17.TenantSource.
const (
	// TenantFromInvitation is the invitation being accepted.
	TenantFromInvitation = "invitation"
	// TenantFromHost is the organization the request's host is bound to.
	TenantFromHost = "host"
	// TenantFromDefault is the caller's own choice (the W17-Org selector),
	// or their sole membership where the auth plugin infers it.
	TenantFromDefault = "default"
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
