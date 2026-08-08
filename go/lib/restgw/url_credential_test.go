package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestStripReservedHeadersMiddleware_ForgedCredentialNeverReachesHandler is
// the load-bearing test of REV-162.
//
// The reserved slot is what tells the auth router "this request carries a
// URL-extracted credential, dispatch it to the URL_TOKEN backend". If a
// client can set that header itself, it can present an arbitrary value to
// the capability-token backend from ANY route on the surface — the exact
// hole the per-endpoint `credential` declaration exists to close.
func TestStripReservedHeadersMiddleware_ForgedCredentialNeverReachesHandler(t *testing.T) {
	var seen string
	var sawKey bool
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(restgw.CredentialHeader)
		_, sawKey = r.Header[restgw.CredentialHeader]
	})
	h := restgw.StripReservedHeadersMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	req.Header.Set(restgw.CredentialHeader, "forged-capability-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "" {
		t.Errorf("handler saw forged credential %q; want it stripped", seen)
	}
	if sawKey {
		t.Error("handler saw the reserved header key at all; want it removed, not blanked — a present-but-empty key still reads as 'the gateway set this'")
	}
}

// TestStripReservedHeadersMiddleware_CanonicalisationVariants: Go canonicalises
// header keys on Set/Get, but a client sends bytes. A scrub keyed on the
// canonical form only would be defeated by nothing here (net/http canonicalises
// on parse), but this pins the behaviour so a future hand-built header map in a
// test or a proxy shim cannot slip a variant through unnoticed.
func TestStripReservedHeadersMiddleware_CanonicalisationVariants(t *testing.T) {
	for _, variant := range []string{
		"x-w17-credential",
		"X-W17-CREDENTIAL",
		"X-w17-Credential",
	} {
		t.Run(variant, func(t *testing.T) {
			var seen string
			h := restgw.StripReservedHeadersMiddleware(http.HandlerFunc(
				func(_ http.ResponseWriter, r *http.Request) {
					seen = r.Header.Get(restgw.CredentialHeader)
				}))
			req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
			req.Header.Set(variant, "forged")
			h.ServeHTTP(httptest.NewRecorder(), req)
			if seen != "" {
				t.Errorf("variant %q slipped through as %q", variant, seen)
			}
		})
	}
}

// TestStripReservedHeadersMiddleware_LeavesOrdinaryRequestsAlone guards the
// common path: the scrub must not clone or disturb the request when no
// reserved header is present, and must not eat unrelated headers.
func TestStripReservedHeadersMiddleware_LeavesOrdinaryRequestsAlone(t *testing.T) {
	var gotAuth, gotTrace string
	h := restgw.StripReservedHeadersMiddleware(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotTrace = r.Header.Get("Traceparent")
		}))
	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Set("Traceparent", "00-trace-span-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotAuth != "Bearer abc" {
		t.Errorf("Authorization = %q, want it untouched", gotAuth)
	}
	if gotTrace != "00-trace-span-01" {
		t.Errorf("Traceparent = %q, want it untouched", gotTrace)
	}
}

// TestStripReservedHeadersMiddleware_DoesNotMutateCallersHeaders pins the
// clone: a middleware that deleted in place would corrupt a header map the
// caller still holds — a class of bug that only shows under a particular
// test ordering, i.e. the worst kind to debug.
func TestStripReservedHeadersMiddleware_DoesNotMutateCallersHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	req.Header.Set(restgw.CredentialHeader, "forged")
	h := restgw.StripReservedHeadersMiddleware(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := req.Header.Get(restgw.CredentialHeader); got != "forged" {
		t.Errorf("caller's own request was mutated (header now %q); the middleware must clone", got)
	}
}

// TestWithURLCredential_RoundTrip covers the injection half: the value lands
// in the reserved slot, HasURLCredential sees it, and the ORIGINAL request
// does not carry it (nothing captured earlier in the chain should observe a
// credential).
func TestWithURLCredential_RoundTrip(t *testing.T) {
	orig := httptest.NewRequest(http.MethodGet, "/public/invoices/tok-abc", nil)
	injected := restgw.WithURLCredential(orig, "tok-abc")

	if got := injected.Header.Get(restgw.CredentialHeader); got != "tok-abc" {
		t.Errorf("injected credential = %q, want %q", got, "tok-abc")
	}
	if !restgw.HasURLCredential(injected) {
		t.Error("HasURLCredential(injected) = false, want true — this is the auth router's dispatch signal")
	}
	if restgw.HasURLCredential(orig) {
		t.Error("the original request carries the credential; WithURLCredential must return a copy")
	}
}

// TestHasURLCredential_EmptyValueIsNotACredential: an empty value must not
// route to the URL_TOKEN backend. Handlers refuse "" before injecting, but a
// present-and-empty header reaching the router would dispatch a credential-less
// request at the capability backend, where an auth method matching on a token
// column could match a row whose token is empty.
func TestHasURLCredential_EmptyValueIsNotACredential(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/public/invoices/", nil)
	req.Header.Set(restgw.CredentialHeader, "")
	if restgw.HasURLCredential(req) {
		t.Error("HasURLCredential = true for an empty value; want false")
	}
	if restgw.HasURLCredential(nil) {
		t.Error("HasURLCredential(nil) = true; want false")
	}
}

// TestURLCredentialFromPath reads through a real chi route so the test
// exercises the same URL-param plumbing the generated handler runs under,
// rather than a hand-stuffed RouteContext that could pass while the real
// mount point does not.
func TestURLCredentialFromPath(t *testing.T) {
	var got string
	r := chi.NewRouter()
	r.Get("/public/invoices/{token}", func(_ http.ResponseWriter, req *http.Request) {
		got = restgw.URLCredentialFromPath(req, "token")
	})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/public/invoices/tok-xyz", nil))
	if got != "tok-xyz" {
		t.Errorf("URLCredentialFromPath = %q, want %q", got, "tok-xyz")
	}

	// A name the template does not carry yields "" — the parser rejects
	// this at compile time, so the runtime contract is just "empty, never
	// a wrong value from a neighbouring segment".
	r2 := chi.NewRouter()
	var mismatched = "unset"
	r2.Get("/public/invoices/{token}", func(_ http.ResponseWriter, req *http.Request) {
		mismatched = restgw.URLCredentialFromPath(req, "secret")
	})
	r2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/public/invoices/tok-xyz", nil))
	if mismatched != "" {
		t.Errorf("unknown path param yielded %q, want empty", mismatched)
	}
}

func TestURLCredentialFromQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/public/invoices?t=tok-qs&other=x", nil)
	if got := restgw.URLCredentialFromQuery(req, "t"); got != "tok-qs" {
		t.Errorf("URLCredentialFromQuery = %q, want %q", got, "tok-qs")
	}
	if got := restgw.URLCredentialFromQuery(req, "absent"); got != "" {
		t.Errorf("absent query param yielded %q, want empty", got)
	}
	if got := restgw.URLCredentialFromQuery(nil, "t"); got != "" {
		t.Errorf("nil request yielded %q, want empty", got)
	}
}

// TestMissingURLCredentialIsUnauthenticated pins the status choice: a
// malformed link and a revoked token must be indistinguishable to the
// caller, so "no credential" is 401, not 400.
func TestMissingURLCredentialIsUnauthenticated(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteAuthError(rec, restgw.ErrMissingURLCredential)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d — a 400 would tell an attacker the link shape was wrong rather than the token", rec.Code, http.StatusUnauthorized)
	}
	if !restgw.IsMissingURLCredential(restgw.ErrMissingURLCredential) {
		t.Error("IsMissingURLCredential did not recognise its own sentinel")
	}
}
