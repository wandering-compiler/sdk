package restgw_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

const (
	pathSecret  = "cap-path-5f1e9c"
	querySecret = "cap-query-8a27d4"
)

// capabilityRoutes is the generated shape of a capability link: the route
// names the token, the handler injects it (and the query one) into the
// reserved slot before anything else reads the request.
func capabilityRoutes(next http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Get("/public/invoices/{token}", func(w http.ResponseWriter, r *http.Request) {
		r = restgw.WithURLCredential(r, restgw.URLCredentialFromPath(r, "token"))
		r = restgw.WithURLCredential(r, restgw.URLCredentialFromQuery(r, "t"))
		next(w, r)
	})
	return r
}

// The span of a capability-link request names neither credential: not the
// path segment, not the query value, in no attribute and not in its name.
func TestURLCredential_NeverReachesTheTrace(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev); _ = tp.Shutdown(t.Context()) })

	h := restgw.ObservabilityMiddleware(capabilityRoutes(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), "gw", restgw.RequestIDConfig{})
	r := httptest.NewRequest(http.MethodGet, "/public/invoices/"+pathSecret+"?t="+querySecret, nil)
	h.ServeHTTP(httptest.NewRecorder(), r)

	spans := sr.Ended()
	if len(spans) == 0 {
		t.Fatal("no span recorded")
	}
	for _, s := range spans {
		seen := s.Name()
		for _, a := range s.Attributes() {
			seen += fmt.Sprintf(" %s=%s", a.Key, a.Value.Emit())
		}
		if strings.Contains(seen, pathSecret) || strings.Contains(seen, querySecret) {
			t.Errorf("span carries a URL credential: %s", seen)
		}
	}
}

// The next-page link of a capability link keeps its credential — the
// holder of the link is the holder of the next page — and adds nothing.
func TestURLCredential_NextPageLinkKeepsIt(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/public/invoices/"+pathSecret+"?t="+querySecret, nil)
	rec := httptest.NewRecorder()
	restgw.SetNextPageLink(rec, r, "c2")
	link := rec.Header().Get("Link")
	if !strings.Contains(link, "/public/invoices/"+pathSecret) || !strings.Contains(link, "t="+querySecret) {
		t.Errorf("Link = %q, want the capability path and query kept", link)
	}
}

// A browser redirect never copies the request's URL credential into the
// Location it sends.
func TestURLCredential_NeverReachesARedirect(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/public/invoices/"+pathSecret+"/pay?t="+querySecret, nil)
	for name, write := range map[string]func(http.ResponseWriter){
		"internal": func(w http.ResponseWriter) { restgw.WriteBrowserRedirect(w, r, "/done", "/", "", "") },
		"external": func(w http.ResponseWriter) {
			restgw.WriteBrowserRedirectExternal(w, r, "https://pay.example.com/return", "/")
		},
	} {
		rec := httptest.NewRecorder()
		write(rec)
		loc := rec.Header().Get("Location")
		if loc == "" {
			t.Errorf("%s: no redirect written (status %d)", name, rec.Code)
		}
		if strings.Contains(loc, pathSecret) || strings.Contains(loc, querySecret) {
			t.Errorf("%s: Location %q carries a URL credential", name, loc)
		}
	}
}

// A panic under a capability link is reported with the credential replaced
// in the request path: the report goes to the error tracker, which is not
// where a bearer secret belongs.
func TestURLCredential_NeverReachesAPanicReport(t *testing.T) {
	ctx, sr, end := tracedContext(t)
	h := restgw.RecoverPanicMiddleware(restgw.ObservabilityMiddleware(capabilityRoutes(func(http.ResponseWriter, *http.Request) {
		panic("boom under a capability link")
	}), "gw", restgw.RequestIDConfig{}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public/invoices/"+pathSecret+"?t="+querySecret, nil).WithContext(ctx))
	end()

	var out string
	for _, s := range sr.Ended() {
		for _, e := range s.Events() {
			for _, a := range e.Attributes {
				out += " " + a.Value.Emit()
			}
		}
	}
	if rec.Code != http.StatusInternalServerError || !strings.Contains(out, "boom under a capability link") {
		t.Fatalf("status %d, report %q; want a reported panic", rec.Code, out)
	}
	if strings.Contains(out, pathSecret) || strings.Contains(out, querySecret) {
		t.Errorf("panic report carries a URL credential: %q", out)
	}
	if !strings.Contains(out, "/public/invoices/{credential}") {
		t.Errorf("panic report does not name the redacted path: %q", out)
	}
}
