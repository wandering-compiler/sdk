package restgw

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func orgRoute(t *testing.T, header string) (*http.Request, error) {
	t.Helper()
	var got *http.Request
	var gotErr error
	r := chi.NewRouter()
	r.Get("/orgs/{org}/members", func(w http.ResponseWriter, req *http.Request) {
		got, gotErr = WithOrgFromPath(req, "org")
	})
	req := httptest.NewRequest(http.MethodGet, "/orgs/acme/members", nil)
	if header != "" {
		req.Header.Set("W17-Org", header)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
	return got, gotErr
}

// The path's org becomes the W17-Org header authentication reads.
func TestWithOrgFromPath_SetsTheHeader(t *testing.T) {
	got, err := orgRoute(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if h := got.Header.Get("W17-Org"); h != "acme" {
		t.Fatalf("W17-Org = %q, want acme", h)
	}
}

// The same org twice is fine; two different ones are refused — acting in
// either would be a guess.
func TestWithOrgFromPath_RefusesAConflictingHeader(t *testing.T) {
	if _, err := orgRoute(t, "acme"); err != nil {
		t.Fatalf("same org in path and header refused: %v", err)
	}
	if _, err := orgRoute(t, "other"); !errors.Is(err, ErrOrgPathConflict) {
		t.Fatalf("err = %v, want ErrOrgPathConflict", err)
	}
}
