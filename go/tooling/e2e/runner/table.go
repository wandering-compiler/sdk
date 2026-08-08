// Package runner is the execution-side library the generated e2erunner
// uses: a pure protocol client (REST over HTTP/WS, MCP) plus the
// scenario executor. It never touches a database. The codegen bakes the
// YAML corpus into Go (`[]Step` literals) at build time, so there is no
// runtime YAML; RunScenario drives the baked steps against a configured
// target, threading one capture scope through a scenario.
package runner

// Endpoint is the baked routing record for one endpoint — what the
// runner needs to turn a gRPC-shaped step (input fields) into a concrete
// transport call. The codegen emits these as Go literals inside each
// Step, from the same endpoint surface the gateway routes on, so the
// test files never restate routing.
type Endpoint struct {
	// Ref is the gRPC ref `<module>.<Service>.<Method>` — the test
	// file's `method:` value and the table key.
	Ref string

	// Transport selects the caller: "rest" or "mcp".
	Transport string

	// AuthRequired tells the runner to inject the reserved
	// `auth.token` capture as the call's credential.
	//
	// Read together with CredentialInURL: authentication is three
	// states, not two. Unauthenticated (`exclude_auth`) has both
	// false; ordinary bearer auth has AuthRequired true; a REV-162
	// capability-link endpoint has BOTH true — it authenticates,
	// but its credential is a path segment or query parameter, so
	// the bearer injection must not happen.
	AuthRequired bool

	// CredentialInURL (REV-162) marks an endpoint whose credential
	// travels in the URL rather than the Authorization header.
	//
	// The runner must NOT inject `auth.token` for these, and must
	// not require one upstream: a capability link is opened by a
	// recipient with no session at all, and a case that quietly
	// sent a Bearer header alongside would be testing a request no
	// real caller ever makes. The token belongs in the step's
	// `input`, where it binds into the path/query like any other
	// field.
	CredentialInURL bool

	// --- REST routing (Transport == "rest") ---

	// HTTPMethod is the verb (GET/POST/PUT/PATCH/DELETE).
	HTTPMethod string

	// PathTemplate is the URL path with `{field}` placeholders, e.g.
	// `/api/v1/projects/{id}`. Placeholders bind to top-level input
	// fields of the same name.
	PathTemplate string

	// PathParams lists the input fields bound into the path. They
	// are removed from the request body.
	PathParams []string

	// QueryParams lists the top-level input fields expanded into the
	// query string. They are removed from the request body.
	QueryParams []string

	// BodyField selects the request body: "" or "*" = the whole
	// request minus path/query fields; a field name = that
	// sub-message only.
	BodyField string

	// RawBodyField names the bytes field that receives the request
	// body VERBATIM (`raw_body_field` on the endpoint). When set, the
	// step's value for that field IS the body — no JSON envelope, no
	// transcode — because an endpoint that exists to preserve bytes
	// cannot be exercised through a client that rewrites them.
	RawBodyField string

	// --- MCP routing (Transport == "mcp") ---

	// ToolName is the MCP tool the ref maps to (`tools/call` name).
	ToolName string
}
