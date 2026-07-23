// Auth middleware shape for generated REST gateways
// (G3-GW-03). Mirrors the protobridge `runtime/auth.go`
// pattern that the conventions document calls out:
//
//   - HTTP request hits the gateway.
//   - Auth function reads ALL request headers, calls the
//     declared `(w17.http_auth_method)` RPC with them
//     packed into the AuthReq's `map<string,string> headers`
//     field (parser-enforced convention).
//   - On success: AuthResp is `proto.Marshal`-ed +
//     base64-encoded into the outgoing gRPC metadata under
//     `x-w17-user`. Downstream handlers `metadata.FromIncomingContext`
//     + `proto.Unmarshal` to recover claims.
//   - On failure: WriteAuthError maps the gRPC error to the
//     canonical HTTP status (401 / 403 / 5xx) via the same
//     code → status table the rest of restgw uses.
//
// Header passthrough is the design call: every credential
// carrier (Authorization Bearer, Cookie, X-API-Key, custom)
// flows through the same surface, the auth method picks. No
// per-credential annotation needed at the gateway layer —
// the auth method's body decides which header(s) it trusts.
//
// Streaming auth (WebSocket / SSE ticket subsystem) is not
// in this MVP — browsers' `new WebSocket()` and `EventSource`
// can't set Authorization, so they need a separate ticket
// flow. Tracked as a follow-up alongside the broader
// streaming-auth work.

package restgw

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// AuthFunc authenticates an HTTP request and returns the
// serialized AuthResp body to thread into outgoing gRPC
// metadata. Generated handlers call this before each
// non-`http_exclude_auth` RPC.
//
// Returning a nil error + nil userData is the no-auth path
// (see [NoAuth]).
type AuthFunc func(ctx context.Context, r *http.Request) ([]byte, error)

// AuthCaller is the per-API contract the generator emits a
// concrete impl of — it knows the AuthReq / AuthResp proto
// types and the upstream gRPC client. The runtime side stays
// type-erased through `proto.Message` so this package
// doesn't depend on the user's proto package.
type AuthCaller interface {
	CallAuth(ctx context.Context, headers map[string]string) (proto.Message, error)
}

// MakeAuthFunc wraps an AuthCaller into an AuthFunc.
// Generated main.go produces an AuthCaller that knows the
// concrete request / response types and threads the gRPC
// client; this helper handles the header → caller →
// proto-marshal → bytes pipeline.
func MakeAuthFunc(caller AuthCaller) AuthFunc {
	return func(ctx context.Context, r *http.Request) ([]byte, error) {
		// Take first value per header — http.Header values
		// are []string, but the auth method's map<string,string>
		// shape only carries one. Multi-valued headers
		// (e.g. comma-joined Cookie) are the auth method's
		// concern to split.
		headers := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[k] = v[0]
			}
		}
		resp, err := caller.CallAuth(ctx, headers)
		if err != nil {
			return nil, err
		}
		return proto.Marshal(resp)
	}
}

// NoAuth returns an AuthFunc that always succeeds with nil
// userData. Used when the proto file declares no
// `(w17.http_auth_method)` — the generated main.go wires
// this so the auth-aware handler shape stays uniform.
func NoAuth() AuthFunc {
	return func(_ context.Context, _ *http.Request) ([]byte, error) {
		return nil, nil
	}
}

// (REV-146 — HasPermission moved to the acllock package. The
// gateway emit now calls `acllock.HasPermission(bits, id)`
// against the bitset wire format. restgw stays focused on
// HTTP-level helpers; bitset semantics live with the lock
// owner.)

// AuthScheme is the canonical credential-scheme name the
// gateway's token-type router dispatches on (REV-146).
// Matches the names declared in `w17pb.TokenType` minus the
// "TOKEN_TYPE_" prefix; emitted code compares against these
// strings to pick which auth method handles a request.
//
// AuthSchemeNone fires when no Authorization header is set or
// the scheme isn't one of the recognised values. The router
// then falls through to ANY-typed methods (if any).
const (
	AuthSchemeNone   = ""
	AuthSchemeBearer = "BEARER"
	AuthSchemeBasic  = "BASIC"
)

// ClassifyAuthScheme inspects the request's Authorization
// header and returns the canonical scheme name the token-type
// router compares against. Case-insensitive match on the
// scheme token (HTTP grammar treats the scheme as a token,
// not a quoted string). No header / unrecognised scheme →
// AuthSchemeNone; the router treats that as "fall through to
// ANY-typed methods".
func ClassifyAuthScheme(r *http.Request) string {
	if r == nil {
		return AuthSchemeNone
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return AuthSchemeNone
	}
	sep := strings.IndexByte(auth, ' ')
	if sep <= 0 {
		return AuthSchemeNone
	}
	scheme := strings.ToLower(auth[:sep])
	switch scheme {
	case "bearer":
		return AuthSchemeBearer
	case "basic":
		return AuthSchemeBasic
	}
	return AuthSchemeNone
}

// WriteForbidden writes the canonical 403 PERMISSION_DENIED
// envelope. Generated handlers call this when the request
// passes authentication but the caller's permission_ids
// doesn't include the required permission ID.
//
// Optional `reason` argument lets callers attach the missing
// permission string for debug visibility:
//
//	restgw.WriteForbidden(w, "tasks.TasksService.GetTask")
//
// Note that this string lands in the public response envelope.
// Production deployments that don't want to leak the permission
// catalog to attackers can wrap this helper (or call
// `WriteError(w, 403, "PERMISSION_DENIED", "forbidden")`
// directly) to keep the message generic.
//
// Empty reason / no argument keeps the legacy "forbidden"
// message for back-compat with code written before REV-146.x.
func WriteForbidden(w http.ResponseWriter, reason ...string) {
	msg := "forbidden"
	if len(reason) > 0 && reason[0] != "" {
		msg = "forbidden: missing permission " + reason[0]
	}
	WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", msg)
}

// WriteUnauthorized writes the canonical 401 UNAUTHENTICATED
// envelope. The token-type router calls this when no declared
// auth method handles the classified credential scheme — the
// request is rejected before any backend is invoked.
//
// Optional `scheme` argument (the classified Authorization
// scheme — "BEARER" / "BASIC" / etc.) lets the response
// pinpoint which credential shape the surface refuses. Empty
// scheme / no argument keeps the legacy generic message.
func WriteUnauthorized(w http.ResponseWriter, scheme ...string) {
	msg := "no auth method handles this credential scheme"
	if len(scheme) > 0 && scheme[0] != "" {
		msg = "no auth method handles credential scheme " + scheme[0]
	}
	WriteError(w, http.StatusUnauthorized, "UNAUTHENTICATED", msg)
}

// userMetadataKey is the outgoing gRPC metadata key carrying
// the base64-encoded AuthResp. Mirrors protobridge's
// `x-protobridge-user` shape — different prefix to avoid
// collisions when both stacks coexist in the same process.
const userMetadataKey = "x-w17-user"

// SetUserMetadata threads the marshaled AuthResp into the
// outgoing gRPC metadata. Downstream handlers recover via
// [GetUserMetadata]. nil / empty userData is a no-op so the
// NoAuth path doesn't pollute metadata.
func SetUserMetadata(ctx context.Context, userData []byte) context.Context {
	if len(userData) == 0 {
		return ctx
	}
	encoded := base64.StdEncoding.EncodeToString(userData)
	md, ok := metadata.FromOutgoingContext(ctx)
	if ok {
		md = md.Copy()
	} else {
		md = metadata.MD{}
	}
	md.Set(userMetadataKey, encoded)
	return metadata.NewOutgoingContext(ctx, md)
}

// GetUserMetadata recovers the AuthResp bytes from incoming
// gRPC metadata + base64-decodes them. Handlers that want
// claims call this + `proto.Unmarshal` into their concrete
// AuthResp type.
//
// Returns (nil, false) when the metadata key is absent
// (NoAuth path or excluded RPC). Empty value is treated as
// "no auth" too — the encoded form for non-empty bytes is
// always a non-empty base64 string.
func GetUserMetadata(ctx context.Context) ([]byte, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, false
	}
	vals := md.Get(userMetadataKey)
	if len(vals) == 0 || vals[0] == "" {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(vals[0])
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// authUserDataCtxKey carries the authenticated principal's raw
// AuthResp bytes on the request context so streaming handlers
// (the w17-events SSE channel) can recover the principal's labels
// for per-principal broadcast filtering without a second auth
// call. RequireAuth stashes it on success.
type authUserDataCtxKey struct{}

// withAuthUserData stashes the marshaled AuthResp on ctx. No-op
// for empty userData (the NoAuth path) so unauthed bundles don't
// carry a phantom principal.
func withAuthUserData(ctx context.Context, userData []byte) context.Context {
	if len(userData) == 0 {
		return ctx
	}
	return context.WithValue(ctx, authUserDataCtxKey{}, userData)
}

// AuthUserDataFromContext recovers the AuthResp bytes RequireAuth
// stashed (the marshaled per-domain AuthResp). Returns nil when
// the request didn't pass through RequireAuth, or the auth method
// is NoAuth. Callers proto.Unmarshal into their concrete AuthResp
// type to read claims / labels.
func AuthUserDataFromContext(ctx context.Context) []byte {
	v, _ := ctx.Value(authUserDataCtxKey{}).([]byte)
	return v
}

// WriteAuthError translates an auth-stage error to the HTTP
// response. gRPC status errors round-trip via
// [WriteGRPCError]; non-status errors land at 500 Internal
// (auth dial failures, marshaling glitches — the consumer
// can't recover from these). Auth methods that want a 401
// surface return `status.Error(codes.Unauthenticated, ...)`.
func WriteAuthError(w http.ResponseWriter, err error) {
	WriteGRPCError(w, err)
}

// RequireAuth gates an HTTP handler behind an AuthFunc. It runs authFn
// on the request; on success it calls next, on failure it writes the
// canonical auth-error envelope (via [WriteAuthError], same as the
// per-RPC handlers) and stops. Used for streaming routes (the
// w17-events SSE pub/sub channel, WS upgrades) that mount directly on
// the router instead of through the per-service handler structs and so
// don't inherit the in-handler auth gate.
//
// Pass the streaming-decorated AuthFunc (see [NewWSAuth]) when the
// surface declares an auth method so browser EventSource / WebSocket
// clients can authenticate via the ?ticket= query param — they can't
// set the Authorization header. With no auth method authFn is [NoAuth]
// and this is a transparent pass-through, so dev / unauthed bundles
// keep the route open.
//
// The SSE handler subscribes to the local bus and makes no per-request
// backend gRPC call, so the returned auth metadata is intentionally
// discarded here — gating, not metadata forwarding, is the job.
func RequireAuth(authFn AuthFunc, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userData, err := authFn(r.Context(), r)
		if err != nil {
			// A missing or invalid ticket is an authentication failure,
			// not a server error. NewWSAuth surfaces these as the plain
			// sentinels ErrWSAuthNoTicket / ErrTicketInvalid (by design —
			// callers do the status mapping), so map them to 401 here
			// instead of letting WriteAuthError → WriteGRPCError fall
			// through to 500. Genuine store/transport failures (which do
			// NOT match ErrTicketInvalid) still get WriteAuthError's
			// 500/503 treatment.
			if errors.Is(err, ErrWSAuthNoTicket) || errors.Is(err, ErrTicketInvalid) {
				WriteUnauthorized(w)
				return
			}
			WriteAuthError(w, err)
			return
		}
		// Stash the principal bytes so streaming handlers (w17-events)
		// can recover the principal's labels for broadcast filtering.
		if len(userData) > 0 {
			r = r.WithContext(withAuthUserData(r.Context(), userData))
		}
		next(w, r)
	}
}

// CachedAuthFunc wraps an inner AuthFunc with an in-memory TTL
// cache (G3-GW-03 cache layer), keyed on the request's
// credential-bearing headers (default: Authorization / Cookie /
// X-Api-Key — see [defaultAuthCacheKeyHeaders]). Hit returns the
// cached userData without calling the inner func.
//
// Equivalent to [CachedAuthFuncWithKeyHeaders] with no extra
// credential headers. Use that form when the surface's auth
// method authenticates on a header outside the default set — see
// its doc for the cross-principal-correctness constraint that
// makes the credential-header set load-bearing.
//
// Zero / negative TTL disables caching entirely and returns
// `inner` unchanged — generators wire this via
// `<PREFIX>_AUTH_CACHE_TTL_SECONDS` env (0 = off, default
// off; opt-in for operator who measures the upstream cost
// and accepts the staleness window).
//
// Eviction is lazy on get — entries past TTL are dropped on
// next access. A periodic GC pass isn't wired (cache is
// header-keyed, so dead entries from rotated tokens fall
// out of memory gradually as the same headers stop arriving).
//
// Errors from `inner` are NOT cached — failure paths
// (network blip, auth service down) re-fire on every
// retry until they either succeed or the request gives up.
func CachedAuthFunc(inner AuthFunc, ttl time.Duration) AuthFunc {
	return CachedAuthFuncWithKeyHeaders(inner, ttl, nil)
}

// CachedAuthFuncWithKeyHeaders is the configurable form of
// [CachedAuthFunc]. extraCredHeaders names additional request
// headers — beyond [defaultAuthCacheKeyHeaders] — that the
// surface's auth method may authenticate on, and that must
// therefore be folded into the cache key.
//
// LOUD CONSTRAINT (correctness, not tuning). The cache key MUST
// cover EVERY header the auth method derives identity from.
// Header passthrough (see [MakeAuthFunc]) forwards ALL request
// headers to the auth method, so the gateway cannot know which
// ones it trusts. If an auth method authenticates on a header
// that is neither a default credential header nor listed here,
// two distinct principals that share the default headers (e.g.
// the same Bearer token but different `X-Tenant-Token`) would
// otherwise collide on one cache slot and be served each other's
// identity. Two defenses keep that from happening silently:
//
//  1. A request carrying ANY header outside the union of the
//     credential-key set and the curated non-credential
//     allowlist ([authCacheKnownNonCredHeaders]) is treated as
//     possibly-credentialed and is NOT cached (cacheable=false →
//     pass straight through to inner). This is the safe default:
//     an unrecognized header costs a cache miss, never a
//     cross-principal leak.
//  2. Listing the auth method's real credential header(s) here
//     (operators wire `<PREFIX>_AUTH_CACHE_CRED_HEADERS`, see
//     [AuthCacheCredHeadersFromEnv]) both folds them into the key
//     AND marks them known — restoring cache hits for that
//     surface without sacrificing correctness.
//
// Net: caching is correct for any auth method out of the box
// (worst case: it just doesn't cache exotic-header requests);
// operators opt exotic-header auth back into the cache by naming
// the header.
func CachedAuthFuncWithKeyHeaders(inner AuthFunc, ttl time.Duration, extraCredHeaders []string) AuthFunc {
	if ttl <= 0 {
		return inner
	}
	keyHeaders := canonicalHeaderSet(defaultAuthCacheKeyHeaders, extraCredHeaders)
	// Precompute the deterministic key-header iteration order ONCE — the set
	// is fixed for the closure's lifetime, so deriving+sorting it per request
	// would burn a slice alloc + sort on the auth fast path the cache exists
	// to keep cheap.
	sortedKeyNames := sortedSetKeys(keyHeaders)
	c := &authCache{ttl: ttl}
	return func(ctx context.Context, r *http.Request) ([]byte, error) {
		key, cacheable := hashHeaders(r.Header, keyHeaders, sortedKeyNames)
		if !cacheable {
			// Either no recognized credential header is present, or the
			// request carries an unrecognized header that could itself be
			// a credential the key doesn't cover. Both cases MUST bypass
			// the cache so distinct principals never share a slot.
			return inner(ctx, r)
		}
		if data, ok := c.get(key); ok {
			return data, nil
		}
		data, err := inner(ctx, r)
		if err != nil {
			return nil, err
		}
		c.put(key, data)
		return data, nil
	}
}

// authCache is the in-memory TTL store for [CachedAuthFunc].
// sync.Map gives lock-free reads + amortised inserts; the
// per-entry expiry is checked on get so no goroutine
// reaper is needed.
type authCache struct {
	m   sync.Map // map[string]authEntry
	ttl time.Duration
}

type authEntry struct {
	data   []byte
	expiry time.Time
}

func (c *authCache) get(key string) ([]byte, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return nil, false
	}
	e := v.(authEntry)
	if time.Now().After(e.expiry) {
		c.m.Delete(key)
		return nil, false
	}
	return e.data, true
}

func (c *authCache) put(key string, data []byte) {
	c.m.Store(key, authEntry{
		data:   data,
		expiry: time.Now().Add(c.ttl),
	})
}

// defaultAuthCacheKeyHeaders are the credential-bearing request headers
// folded into the auth-cache key by default. Keying on the FULL header
// set drove the cache hit rate to ~zero, because the default propagated
// set includes per-request volatile headers (X-Request-Id, traceparent,
// tracestate, baggage) that change on every call: the same credential
// almost never produced the same key. Restricting the key to the
// credential headers means repeated calls with the same credential reuse
// the cached result, which is the whole point of the cache. Surfaces
// whose auth method trusts a header beyond this set extend it via
// [CachedAuthFuncWithKeyHeaders] / `<PREFIX>_AUTH_CACHE_CRED_HEADERS`.
var defaultAuthCacheKeyHeaders = []string{"Authorization", "Cookie", "X-Api-Key"}

// authCacheKnownNonCredHeaders is the curated allowlist of request
// headers KNOWN not to carry a credential — standard representation /
// conditional / caching / connection / proxy-forwarding / tracing /
// browser fetch-metadata headers. A cached request is allowed to carry
// any of these (they're ignored for keying); a request carrying a header
// in NEITHER this set NOR the credential-key set is treated as
// possibly-credentialed and bypasses the cache (see [hashHeaders]).
//
// The list is best-effort: an omission only costs a cache MISS (the
// safe direction), never a cross-principal leak. Names are stored
// canonicalised (http.CanonicalHeaderKey) so lookups against r.Header's
// already-canonical keys are exact.
var authCacheKnownNonCredHeaders = canonicalHeaderSet([]string{
	// Representation / content negotiation.
	"Accept", "Accept-Charset", "Accept-Encoding", "Accept-Language",
	"Accept-Datetime", "Content-Type", "Content-Length", "Content-Encoding",
	"Content-Language", "Content-Md5", "Content-Disposition", "Mime-Version",
	// Conditional + range (vary per request, never credentials).
	"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since",
	"If-Range", "Range",
	// Caching.
	"Cache-Control", "Pragma", "Date",
	// Connection / transport.
	"Connection", "Keep-Alive", "Upgrade", "Upgrade-Insecure-Requests",
	"Transfer-Encoding", "Te", "Trailer", "Expect", "Max-Forwards", "Via",
	// Client / agent identity (not credentials).
	"User-Agent", "Referer", "From", "Origin", "Host", "Dnt", "Sec-Gpc",
	"Accept-Ch",
	// Proxy / forwarding.
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	"X-Forwarded-Port", "X-Forwarded-Server", "X-Real-Ip",
	"X-Original-Forwarded-For",
	// Tracing / correlation (the per-request-volatile set that motivated
	// keying on credentials only in the first place).
	"X-Request-Id", "X-Correlation-Id", "X-Trace-Id", "X-Amzn-Trace-Id",
	"Traceparent", "Tracestate", "Baggage", "X-B3-Traceid", "X-B3-Spanid",
	"X-B3-Parentspanid", "X-B3-Sampled", "X-B3-Flags",
	// Browser fetch metadata / client hints.
	"Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site", "Sec-Fetch-User",
	"Sec-Ch-Ua", "Sec-Ch-Ua-Mobile", "Sec-Ch-Ua-Platform",
	"Sec-Ch-Ua-Platform-Version", "Sec-Ch-Ua-Arch", "Sec-Ch-Ua-Bitness",
	"Sec-Ch-Ua-Full-Version", "Sec-Ch-Ua-Full-Version-List",
	"Sec-Ch-Ua-Model", "Priority", "Save-Data", "Device-Memory", "Dpr",
	"Viewport-Width", "Width", "Downlink", "Ect", "Rtt",
})

// canonicalHeaderSet builds a set of canonicalised header names from the
// given groups. Used for both the credential-key set (defaults + operator
// extras) and the known-non-credential allowlist.
func canonicalHeaderSet(groups ...[]string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, g := range groups {
		for _, h := range g {
			set[http.CanonicalHeaderKey(h)] = struct{}{}
		}
	}
	return set
}

// sortedSetKeys returns the set's keys in ascending order. Used to
// precompute the auth-cache key-header iteration order once (it's stable
// for the cache closure's lifetime) instead of per request.
func sortedSetKeys(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// hashHeaders builds a stable cache key from the request's
// credential-bearing headers (keyHeaders) and reports whether the
// request is cacheable. r.Header keys arrive canonicalised, so the
// fixed-order iteration over the sorted key set is deterministic.
//
// cacheable is FALSE when either:
//
//   - none of the credential headers in keyHeaders is present (anonymous
//     / unknown-scheme request — keying it would collapse distinct
//     principals onto one empty key), or
//   - the request carries any header that is NOT in keyHeaders and NOT in
//     the curated non-credential allowlist ([authCacheKnownNonCredHeaders]).
//     Such a header could be a credential the key doesn't cover, so the
//     request is served uncached rather than risk a cross-principal hit.
//
// Multi-valued headers fold into the key by joining values with NUL (a
// byte that can't appear in HTTP header values per RFC 9110 §5.5, so it
// doesn't risk a collision between "a, b" and ["a", "b"]).
func hashHeaders(h http.Header, keyHeaders map[string]struct{}, sortedNames []string) (key string, cacheable bool) {
	// Reject up front if any header is neither a credential-key header nor
	// a known-safe one — it might be an unaccounted credential.
	for name := range h {
		canon := http.CanonicalHeaderKey(name)
		if _, ok := keyHeaders[canon]; ok {
			continue
		}
		if _, ok := authCacheKnownNonCredHeaders[canon]; ok {
			continue
		}
		return "", false
	}

	// sortedNames is the credential-key set in deterministic order,
	// precomputed once by the caller (the set is fixed for the closure's
	// lifetime) so the hot path neither allocates nor sorts per request.
	hasher := sha256.New()
	any := false
	for _, k := range sortedNames {
		vals := h.Values(k)
		if len(vals) == 0 {
			continue
		}
		any = true
		_, _ = hasher.Write([]byte(k))
		_, _ = hasher.Write([]byte{0})
		for i, v := range vals {
			if i > 0 {
				_, _ = hasher.Write([]byte{0})
			}
			_, _ = hasher.Write([]byte(v))
		}
		_, _ = hasher.Write([]byte{0, 0})
	}
	if !any {
		return "", false
	}
	sum := hasher.Sum(nil)
	return base64.RawStdEncoding.EncodeToString(sum), true
}

// AuthCacheCredHeadersFromEnv reads `<PREFIX>_AUTH_CACHE_CRED_HEADERS`
// (a comma-separated list of header names) and returns the extra
// credential headers to thread into [CachedAuthFuncWithKeyHeaders].
// Empty / unset returns nil — the default credential-key set applies.
//
// Wire this when the surface's auth method authenticates on a header
// beyond Authorization / Cookie / X-Api-Key (e.g. `X-Session-Token`):
// naming it here both keeps the cache correct (folds it into the key)
// and restores cache hits for that surface (the header stops being
// treated as an unknown, cache-bypassing one).
func AuthCacheCredHeadersFromEnv(prefix string, lookup func(string) string) []string {
	return splitHeaderCSV(lookup(prefix + "_AUTH_CACHE_CRED_HEADERS"))
}

// splitHeaderCSV parses a comma-separated header-name list, trimming
// blanks. Local to auth.go to avoid a cross-file dependency on cors.go's
// splitCSV (same shape; kept separate so the two can diverge).
func splitHeaderCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// AuthCacheTTLFromEnv reads `<PREFIX>_AUTH_CACHE_TTL_SECONDS`
// off `lookup` and returns the parsed duration. Zero / unset
// / unparseable / negative all map to 0 (caching disabled) —
// the generator wires `CachedAuthFunc(inner, ttl)` and the
// helper short-circuits at ttl <= 0, so unset env stays
// pass-through.
func AuthCacheTTLFromEnv(prefix string, lookup func(string) string) time.Duration {
	raw := lookup(prefix + "_AUTH_CACHE_TTL_SECONDS")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// uint64BE — kept for future cache-stat instrumentation
// where a stable 64-bit fingerprint of the headers is more
// useful than the base64 string. Currently unused; lift
// when an /debug/auth-cache endpoint lands.
func uint64BE(b []byte) uint64 {
	if len(b) < 8 {
		var pad [8]byte
		copy(pad[:], b)
		return binary.BigEndian.Uint64(pad[:])
	}
	return binary.BigEndian.Uint64(b[:8])
}

var _ = uint64BE // silence unused — see comment above
