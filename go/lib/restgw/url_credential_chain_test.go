package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The chain a capability link runs through: the edge scrub, then the
// handler-side injection of the URL value into the credential slot, then
// the auth router, which dispatches to the URL_TOKEN method whenever the
// slot is filled. A client that forges X-W17-Credential on a route whose
// URL carries no token MUST NOT reach that method with its forged value,
// and on a capability route the auth sees the URL's value, not the
// forged one.
func TestURLCredential_ForgedSlotNeverReachesTheURLTokenMethod(t *testing.T) {
	var sawURLToken []string
	auth := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		if restgw.HasURLCredential(r) {
			sawURLToken = append(sawURLToken, r.Header.Get(restgw.CredentialHeader))
		}
		return []byte("u"), nil
	})
	router := chi.NewRouter()
	// A capability route: the handler injects the path value.
	router.Get("/public/invoices/{token}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = auth(r.Context(), restgw.WithURLCredential(r, restgw.URLCredentialFromPath(r, "token")))
	})
	// An ordinary route: the handler authenticates the request as it came.
	router.Get("/v1/notes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = auth(r.Context(), r)
	})
	h := restgw.StripReservedHeadersMiddleware(router)

	for _, forged := range []string{"X-W17-Credential", "x-w17-credential", "X-W17-CREDENTIAL"} {
		sawURLToken = nil
		r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)
		r.Header[forged] = []string{"forged-token"}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if len(sawURLToken) != 0 {
			t.Fatalf("%s on an ordinary route reached the URL_TOKEN method with %q", forged, sawURLToken)
		}

		r = httptest.NewRequest(http.MethodGet, "/public/invoices/real-token", nil)
		r.Header[forged] = []string{"forged-token"}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if len(sawURLToken) != 1 || sawURLToken[0] != "real-token" {
			t.Fatalf("%s on a capability route: the URL_TOKEN method saw %q, want the URL's real-token", forged, sawURLToken)
		}
	}
}
