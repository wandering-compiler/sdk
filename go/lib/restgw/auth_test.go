package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// (REV-146 — HasPermission tests live with the lock package
// at acllock's TestHasPermission. The
// bitset shape is acllock's concern; restgw stays focused on
// HTTP-level helpers.)

// REV-146: ClassifyAuthScheme reads the Authorization scheme
// case-insensitively. No header / unrecognised scheme falls
// through to AuthSchemeNone so the token-type router lands on
// the ANY catch-all (when configured) or returns 401.
func TestClassifyAuthScheme(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"bearer lowercase", "bearer abc.def", restgw.AuthSchemeBearer},
		{"bearer capitalised", "Bearer abc.def", restgw.AuthSchemeBearer},
		{"bearer SHOUT", "BEARER abc.def", restgw.AuthSchemeBearer},
		{"basic", "Basic dXNlcjpwYXNz", restgw.AuthSchemeBasic},
		{"unknown scheme", "Digest abc", restgw.AuthSchemeNone},
		{"no space", "Bearerabc", restgw.AuthSchemeNone},
		{"empty", "", restgw.AuthSchemeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			got := restgw.ClassifyAuthScheme(r)
			if got != c.want {
				t.Errorf("header=%q → %q, want %q", c.header, got, c.want)
			}
		})
	}
}

// REV-146: WriteForbidden writes 403 PERMISSION_DENIED with
// the canonical error envelope. Pin the envelope shape so
// generated handlers can rely on it.
//
// Without `reason` → generic "forbidden" message (back-compat
// with pre-REV-146.x callers). With `reason` → message
// includes the missing permission string for debug visibility.
func TestWriteForbidden(t *testing.T) {
	t.Run("no reason", func(t *testing.T) {
		rec := httptest.NewRecorder()
		restgw.WriteForbidden(rec)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "PERMISSION_DENIED") {
			t.Errorf("body = %q, want PERMISSION_DENIED", body)
		}
		if !strings.Contains(body, "forbidden") {
			t.Errorf("body = %q, want generic 'forbidden' message", body)
		}
	})
	t.Run("with perm string", func(t *testing.T) {
		rec := httptest.NewRecorder()
		restgw.WriteForbidden(rec, "tasks.Task#delete")
		body := rec.Body.String()
		if !strings.Contains(body, "tasks.Task#delete") {
			t.Errorf("body = %q, want missing-perm detail", body)
		}
	})
}

// REV-146: WriteUnauthorized writes 401 UNAUTHENTICATED.
// Optional scheme arg surfaces in the message for debug.
func TestWriteUnauthorized(t *testing.T) {
	t.Run("no scheme", func(t *testing.T) {
		rec := httptest.NewRecorder()
		restgw.WriteUnauthorized(rec)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "UNAUTHENTICATED") {
			t.Errorf("body = %q, want UNAUTHENTICATED", body)
		}
	})
	t.Run("with scheme", func(t *testing.T) {
		rec := httptest.NewRecorder()
		restgw.WriteUnauthorized(rec, "BEARER")
		body := rec.Body.String()
		if !strings.Contains(body, "BEARER") {
			t.Errorf("body = %q, want scheme detail", body)
		}
	})
}

// G3-GW-03: NoAuth returns nil userData + nil err. Generators
// wire this when no (w17.http_auth_method) is declared so the
// handler shape stays uniform.
func TestAuth_NoAuth_NilDataNilErr(t *testing.T) {
	fn := restgw.NoAuth()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	data, err := fn(context.Background(), r)
	if err != nil || data != nil {
		t.Errorf("NoAuth returned (%v, %v); want (nil, nil)", data, err)
	}
}

// G3-GW-03: MakeAuthFunc passes every HTTP header into the
// caller as map<string,string>. Multi-valued headers fold to
// the first value (auth method's body splits if it cares).
type capturingCaller struct {
	resp     proto.Message
	captured map[string]string
}

func (c *capturingCaller) CallAuth(_ context.Context, headers map[string]string) (proto.Message, error) {
	c.captured = headers
	return c.resp, nil
}

func TestAuth_MakeAuthFunc_HeadersForwarded(t *testing.T) {
	caller := &capturingCaller{resp: wrapperspb.String("user-42")}
	fn := restgw.MakeAuthFunc(caller)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer abc.def.ghi")
	r.Header.Set("Cookie", "session=xyz")
	r.Header.Set("X-Tenant-Id", "acme")

	data, err := fn(context.Background(), r)
	if err != nil {
		t.Fatalf("AuthFunc: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty marshaled AuthResp")
	}
	// Round-trip: bytes unmarshal back into the wrapper.
	var got wrapperspb.StringValue
	if err := proto.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Value != "user-42" {
		t.Errorf("AuthResp Value = %q, want user-42", got.Value)
	}
	// Captured headers carry every key the caller set.
	if caller.captured["Authorization"] != "Bearer abc.def.ghi" {
		t.Errorf("captured Authorization = %q", caller.captured["Authorization"])
	}
	if caller.captured["Cookie"] != "session=xyz" {
		t.Errorf("captured Cookie = %q", caller.captured["Cookie"])
	}
	if caller.captured["X-Tenant-Id"] != "acme" {
		t.Errorf("captured X-Tenant-Id = %q", caller.captured["X-Tenant-Id"])
	}
}

// G3-GW-03: caller error propagates unchanged. The handler-
// side WriteAuthError translates it to the canonical HTTP
// status (this test pins the inner contract; the HTTP-side
// translation is covered by the existing WriteGRPCError
// tests).
type erroringCaller struct{}

func (erroringCaller) CallAuth(_ context.Context, _ map[string]string) (proto.Message, error) {
	return nil, status.Error(codes.Unauthenticated, "bad token")
}

func TestAuth_MakeAuthFunc_ErrorPropagates(t *testing.T) {
	fn := restgw.MakeAuthFunc(erroringCaller{})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := fn(context.Background(), r)
	if err == nil {
		t.Fatal("expected error from caller")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Errorf("err code = %v, want Unauthenticated", st.Code())
	}
}

// G3-GW-03: SetUserMetadata threads marshaled bytes into
// outgoing gRPC metadata under x-w17-user. GetUserMetadata
// recovers the bytes from incoming metadata. Round-trip
// pins the (set → grpc-server-side recover) contract.
func TestAuth_SetGetUserMetadata_RoundTrip(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0xff, 0x00, 0x42}

	// Outgoing side (gateway → backend).
	ctx := restgw.SetUserMetadata(context.Background(), original)
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("outgoing metadata missing after SetUserMetadata")
	}
	vals := md.Get("x-w17-user")
	if len(vals) != 1 {
		t.Fatalf("x-w17-user count = %d, want 1", len(vals))
	}
	// Encoded form is base64 — never contains the raw 0xff
	// or 0x00 bytes that would break gRPC's text metadata.
	if strings.ContainsRune(vals[0], '\x00') {
		t.Errorf("encoded value carries raw NUL byte: %q", vals[0])
	}

	// Incoming side (backend handler) — simulate by building
	// an incoming metadata context from the encoded value.
	in := metadata.New(map[string]string{"x-w17-user": vals[0]})
	got, ok := restgw.GetUserMetadata(metadata.NewIncomingContext(context.Background(), in))
	if !ok {
		t.Fatal("GetUserMetadata returned ok=false on populated metadata")
	}
	if string(got) != string(original) {
		t.Errorf("recovered bytes = %v, want %v", got, original)
	}
}

// G3-GW-03: nil / empty userData is a no-op — NoAuth path
// flows through SetUserMetadata without polluting metadata.
func TestAuth_SetUserMetadata_NilNoOp(t *testing.T) {
	ctx := restgw.SetUserMetadata(context.Background(), nil)
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if vals := md.Get("x-w17-user"); len(vals) != 0 {
			t.Errorf("nil userData populated metadata: %v", vals)
		}
	}
}

// G3-GW-03: GetUserMetadata returns (nil, false) on absent
// or undecodable metadata.
func TestAuth_GetUserMetadata_AbsentOrBad(t *testing.T) {
	if _, ok := restgw.GetUserMetadata(context.Background()); ok {
		t.Error("expected ok=false on no incoming metadata")
	}
	in := metadata.New(map[string]string{"x-w17-user": "not-base64-!!!"})
	if _, ok := restgw.GetUserMetadata(metadata.NewIncomingContext(context.Background(), in)); ok {
		t.Error("expected ok=false on undecodable base64")
	}
}

// G3-GW-03 cache: identical request hits cache after first
// call. Counter on the inner func proves the cache short-
// circuits the upstream RPC.
func TestAuth_CachedAuthFunc_Hit(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, _ *http.Request) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("ok"), nil
	})
	cached := restgw.CachedAuthFunc(inner, 1*time.Minute)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer abc")

	for i := 0; i < 5; i++ {
		data, err := cached(context.Background(), r)
		if err != nil || string(data) != "ok" {
			t.Fatalf("call %d: (%q, %v)", i, data, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("inner calls = %d, want 1 (cache hits)", got)
	}
}

// G3-GW-03 cache: distinct credentials never share a cache
// slot — the hash is keyed on every header → value pair.
func TestAuth_CachedAuthFunc_DistinctHeadersDistinctSlots(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte(r.Header.Get("Authorization")), nil
	})
	cached := restgw.CachedAuthFunc(inner, 1*time.Minute)

	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.Header.Set("Authorization", "Bearer aaa")
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "Bearer bbb")

	d1, _ := cached(context.Background(), r1)
	d2, _ := cached(context.Background(), r2)
	if string(d1) != "Bearer aaa" || string(d2) != "Bearer bbb" {
		t.Errorf("distinct creds returned same data: %q / %q", d1, d2)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("inner calls = %d, want 2 (distinct slots)", got)
	}
}

// restgw-sec-1 regression: an auth method may trust a credential in a
// header outside the recognized {Authorization, Cookie, X-Api-Key} set
// (e.g. X-Session-Token). Such requests must NEVER share a cache slot —
// otherwise two distinct principals collapse to one key and the second
// is served the first's identity. The cache must be bypassed (inner
// called every time) when no recognized credential header is present.
func TestAuth_CachedAuthFunc_CustomCredentialHeader_NoCrossPrincipal(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte(r.Header.Get("X-Session-Token")), nil
	})
	cached := restgw.CachedAuthFunc(inner, 1*time.Minute)

	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.Header.Set("X-Session-Token", "principal-A")
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("X-Session-Token", "principal-B")

	d1, _ := cached(context.Background(), r1)
	d2, _ := cached(context.Background(), r2)
	if string(d1) != "principal-A" || string(d2) != "principal-B" {
		t.Errorf("cross-principal cache confusion: A got %q, B got %q", d1, d2)
	}
	// Re-issue A: must still resolve A (not a stale shared slot) — bypass
	// means inner is called every time for custom-header credentials.
	d1again, _ := cached(context.Background(), r1)
	if string(d1again) != "principal-A" {
		t.Errorf("re-issued A got %q, want principal-A", d1again)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("inner calls = %d, want 3 (cache bypassed for custom-header creds)", got)
	}
}

// G3-GW-03 cache: zero / negative TTL returns the inner
// AuthFunc unchanged (no caching layer at all).
func TestAuth_CachedAuthFunc_ZeroTTL_Passthrough(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, _ *http.Request) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("ok"), nil
	})
	cached := restgw.CachedAuthFunc(inner, 0)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for i := 0; i < 3; i++ {
		_, _ = cached(context.Background(), r)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("inner calls = %d, want 3 (no caching at zero TTL)", got)
	}
}

// G3-GW-03 cache: errors are NOT cached — failure path
// re-fires on every retry until success populates the cache.
// First two calls fail, third succeeds + caches; calls 4 and
// 5 hit cache (counter stays at 3).
func TestAuth_CachedAuthFunc_ErrorsNotCached(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, _ *http.Request) ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			return nil, errors.New("transient")
		}
		return []byte("ok"), nil
	})
	cached := restgw.CachedAuthFunc(inner, 1*time.Minute)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// A recognized credential header makes the request cacheable (a
	// credential-less request now bypasses the cache by design — see
	// restgw-sec-1).
	r.Header.Set("Authorization", "Bearer x")

	// Calls 1 + 2 fail, call 3 succeeds + caches.
	_, e1 := cached(context.Background(), r)
	_, e2 := cached(context.Background(), r)
	d3, e3 := cached(context.Background(), r)
	if e1 == nil || e2 == nil {
		t.Errorf("expected first two calls to error: e1=%v e2=%v", e1, e2)
	}
	if e3 != nil || string(d3) != "ok" {
		t.Errorf("expected third call to succeed; got (%q, %v)", d3, e3)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("inner calls after first success = %d, want 3", got)
	}
	// Calls 4 + 5 hit the cache — counter stays at 3.
	_, _ = cached(context.Background(), r)
	_, _ = cached(context.Background(), r)
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("inner calls after cache hits = %d, want 3 (success cached)", got)
	}
}

// G3-GW-03 cache: TTL expiry evicts the entry.
func TestAuth_CachedAuthFunc_TTLExpiry(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, _ *http.Request) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("ok"), nil
	})
	cached := restgw.CachedAuthFunc(inner, 50*time.Millisecond)
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	_, _ = cached(context.Background(), r)
	time.Sleep(80 * time.Millisecond)
	_, _ = cached(context.Background(), r)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("inner calls = %d, want 2 (TTL expired between)", got)
	}
}

// G3-GW-03 env helper: positive int → duration; everything
// else (unset / zero / negative / non-numeric) → 0 (off).
func TestAuth_AuthCacheTTLFromEnv(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"abc", 0},
		{"60", 60 * time.Second},
		{"600", 600 * time.Second},
	}
	for _, c := range cases {
		got := restgw.AuthCacheTTLFromEnv("X", func(k string) string {
			if k == "X_AUTH_CACHE_TTL_SECONDS" {
				return c.raw
			}
			return ""
		})
		if got != c.want {
			t.Errorf("raw=%q → %s, want %s", c.raw, got, c.want)
		}
	}
}

// R-restgw-3 — the dangerous collision: two principals share the
// default credential header (Authorization) but differ in an
// UNACCOUNTED header the auth method actually trusts (X-Tenant-Token).
// Keying only on Authorization would serve them each other's identity.
// The fix bypasses the cache whenever a request carries a header that is
// neither a credential-key header nor a known non-credential one, so the
// unaccounted header forces a per-request inner call — no leak.
func TestAuth_CachedAuthFunc_UnknownHeaderBypassesCache(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte(r.Header.Get("X-Tenant-Token")), nil
	})
	cached := restgw.CachedAuthFunc(inner, time.Minute)

	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.Header.Set("Authorization", "Bearer shared")
	r1.Header.Set("X-Tenant-Token", "tenant-A")
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "Bearer shared")
	r2.Header.Set("X-Tenant-Token", "tenant-B")

	d1, _ := cached(context.Background(), r1)
	d2, _ := cached(context.Background(), r2)
	if string(d1) != "tenant-A" || string(d2) != "tenant-B" {
		t.Errorf("cross-principal leak: A=%q B=%q", d1, d2)
	}
	d1again, _ := cached(context.Background(), r1)
	if string(d1again) != "tenant-A" {
		t.Errorf("re-issued A got %q, want tenant-A", d1again)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("inner calls = %d, want 3 (unknown header bypasses cache)", got)
	}
}

// R-restgw-3 — known non-credential headers (Accept, User-Agent) and
// per-request volatile ones (X-Request-Id) must NOT trip the bypass and
// must NOT enter the key: two requests with the same credential but
// different safe/volatile headers still hit one cache slot.
func TestAuth_CachedAuthFunc_KnownSafeHeadersStillCache(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, _ *http.Request) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("ok"), nil
	})
	cached := restgw.CachedAuthFunc(inner, time.Minute)

	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.Header.Set("Authorization", "Bearer abc")
	r1.Header.Set("Accept", "application/json")
	r1.Header.Set("X-Request-Id", "req-1")
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "Bearer abc")
	r2.Header.Set("User-Agent", "curl/8.0")
	r2.Header.Set("X-Request-Id", "req-2")

	_, _ = cached(context.Background(), r1)
	_, _ = cached(context.Background(), r2)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("inner calls = %d, want 1 (safe/volatile headers ignored for keying)", got)
	}
}

// R-restgw-3 — naming the auth method's real credential header via
// CachedAuthFuncWithKeyHeaders both keeps the cache correct (distinct
// values → distinct slots, no cross-principal hit) AND restores cache
// hits for that surface (re-issuing the same principal hits the cache).
func TestAuth_CachedAuthFuncWithKeyHeaders_CustomCredHeader(t *testing.T) {
	var calls int32
	inner := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte(r.Header.Get("X-Tenant-Token")), nil
	})
	cached := restgw.CachedAuthFuncWithKeyHeaders(inner, time.Minute, []string{"X-Tenant-Token"})

	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.Header.Set("Authorization", "Bearer shared")
	r1.Header.Set("X-Tenant-Token", "tenant-A")
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "Bearer shared")
	r2.Header.Set("X-Tenant-Token", "tenant-B")

	d1, _ := cached(context.Background(), r1)
	d2, _ := cached(context.Background(), r2)
	if string(d1) != "tenant-A" || string(d2) != "tenant-B" {
		t.Errorf("custom-cred collision: A=%q B=%q", d1, d2)
	}
	// Re-issue A: served from cache now (the header is in the key).
	d1again, _ := cached(context.Background(), r1)
	if string(d1again) != "tenant-A" {
		t.Errorf("re-issued A got %q, want tenant-A", d1again)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("inner calls = %d, want 2 (distinct slots, each principal cached)", got)
	}
}

// R-restgw-3 — env helper parses the CSV credential-header list,
// trimming blanks; unset → nil.
func TestAuth_AuthCacheCredHeadersFromEnv(t *testing.T) {
	got := restgw.AuthCacheCredHeadersFromEnv("X", func(k string) string {
		if k == "X_AUTH_CACHE_CRED_HEADERS" {
			return "X-Tenant-Token, X-Session-Token ,,"
		}
		return ""
	})
	want := []string{"X-Tenant-Token", "X-Session-Token"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := restgw.AuthCacheCredHeadersFromEnv("X", func(string) string { return "" }); got != nil {
		t.Errorf("unset → %v, want nil", got)
	}
}
