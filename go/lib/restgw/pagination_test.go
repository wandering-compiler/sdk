package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func TestSetNextPageLink_BasicHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/users", nil)
	rr := httptest.NewRecorder()

	restgw.SetNextPageLink(rr, req, "abc123")

	got := rr.Header().Get("Link")
	want := `<http://api.example.com/users?cursor=abc123>; rel="next"`
	if got != want {
		t.Errorf("Link header\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSetNextPageLink_PreservesExistingQuery(t *testing.T) {
	// Existing filter / sort / limit query params stay on
	// the next-page URL — only `cursor` is replaced.
	req := httptest.NewRequest(http.MethodGet,
		"http://api.example.com/users?country=CZ&limit=20&sort=created_at", nil)
	rr := httptest.NewRecorder()

	restgw.SetNextPageLink(rr, req, "page2")

	got := rr.Header().Get("Link")
	if !strings.Contains(got, "country=CZ") {
		t.Errorf("missing country=CZ:\n%s", got)
	}
	if !strings.Contains(got, "limit=20") {
		t.Errorf("missing limit=20:\n%s", got)
	}
	if !strings.Contains(got, "sort=created_at") {
		t.Errorf("missing sort=created_at:\n%s", got)
	}
	if !strings.Contains(got, "cursor=page2") {
		t.Errorf("missing cursor=page2:\n%s", got)
	}
}

func TestSetNextPageLink_ReplacesExistingCursor(t *testing.T) {
	// User on page 2 fetches page 3 — old cursor must be
	// replaced, not appended.
	req := httptest.NewRequest(http.MethodGet,
		"http://api.example.com/users?cursor=page2", nil)
	rr := httptest.NewRecorder()

	restgw.SetNextPageLink(rr, req, "page3")

	got := rr.Header().Get("Link")
	if strings.Contains(got, "page2") {
		t.Errorf("old cursor leaked into next link: %q", got)
	}
	if !strings.Contains(got, "cursor=page3") {
		t.Errorf("new cursor missing: %q", got)
	}
}

func TestSetNextPageLink_EmptyCursor_NoHeader(t *testing.T) {
	// End of pagination — cursor empty → no Link header
	// emitted. Callers can unconditionally invoke
	// SetNextPageLink without an `if cursor != ""` check.
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/users", nil)
	rr := httptest.NewRecorder()

	restgw.SetNextPageLink(rr, req, "")

	if got := rr.Header().Get("Link"); got != "" {
		t.Errorf("empty cursor should NOT set Link; got %q", got)
	}
}

func TestSetNextPageLink_HonoursXForwardedProto(t *testing.T) {
	// Behind a TLS-terminating LB the gateway sees plaintext
	// HTTP but X-Forwarded-Proto: https — the next-page URL
	// must keep the public-facing scheme.
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/users", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()

	restgw.SetNextPageLink(rr, req, "abc")

	if got := rr.Header().Get("Link"); !strings.HasPrefix(got, "<https://") {
		t.Errorf("expected https scheme from X-Forwarded-Proto; got %q", got)
	}
}

func TestSetPrevPageLink_AppendsToExistingNext(t *testing.T) {
	// API that surfaces both directions — Link header
	// concatenates per RFC 5988, comma-separated.
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/users", nil)
	rr := httptest.NewRecorder()

	restgw.SetNextPageLink(rr, req, "n3")
	restgw.SetPrevPageLink(rr, req, "p1")

	got := rr.Header().Get("Link")
	if !strings.Contains(got, `rel="next"`) {
		t.Errorf("missing next rel: %q", got)
	}
	if !strings.Contains(got, `rel="prev"`) {
		t.Errorf("missing prev rel: %q", got)
	}
	if !strings.Contains(got, ", ") {
		t.Errorf("expected comma separator between links: %q", got)
	}
}
