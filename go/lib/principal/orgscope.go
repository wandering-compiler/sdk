package principal

// OrgScopeHeader is the request header a browser or CLI sets to name the
// organization it wants to act in.
//
// CLIENT-supplied and therefore UNTRUSTED, which is what separates it from the
// `x-w17-*` keys [IsGatewayOwnedKey] refuses: the caller names a slug, the auth
// plugin validates membership, and only then is the trusted
// `x-w17-scope-org_id` stamped. A caller can ask to act in an org; it cannot
// assert that it may.
//
// It lives here, in the SDK, because TWO components must agree on it and they
// cannot see each other: the auth plugin READS it, and the REST gateway must
// ADVERTISE it in its CORS preflight. When only one of them knew the string,
// every org-scoped REST call failed in the browser as an opaque
// "Failed to fetch" — the preflight answered 204 with
// `Access-Control-Allow-Headers: Content-Type, Authorization`, so the browser
// refused to send the real request and no log anywhere recorded an attempt
// (w17.app, 2026-09-11).
const OrgScopeHeader = "w17-org"
