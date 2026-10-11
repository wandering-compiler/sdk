package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The ticket issuer, the download handler and the w17-events route answer
// errors in the same JSON envelope as every other route, so a client that
// parses every error one way keeps working on them — and none of them puts
// internal detail in front of the client.

type failingTicketStore struct{ err error }

func (s failingTicketStore) Issue(context.Context, map[string]string, time.Duration) (string, error) {
	return "", s.err
}

func (s failingTicketStore) Redeem(context.Context, string) (map[string]string, error) {
	return nil, s.err
}

func TestTicketIssuer_ErrorsAreTheEnvelope(t *testing.T) {
	okPrincipal := func(*http.Request) (map[string]string, error) {
		return map[string]string{"k": "v"}, nil
	}
	cases := []struct {
		name      string
		method    string
		principal func(*http.Request) (map[string]string, error)
		store     restgw.TicketStore
		status    int
		code      string
		leaks     []string
	}{
		{"wrong method", http.MethodGet, okPrincipal, restgw.NewMemoryTicketStore(), http.StatusMethodNotAllowed, "UNIMPLEMENTED", nil},
		{"no credential", http.MethodPost, func(*http.Request) (map[string]string, error) {
			return nil, restgw.ErrWSAuthTicketNoHeader
		}, restgw.NewMemoryTicketStore(), http.StatusUnauthorized, "UNAUTHENTICATED", []string{"restgw:", "Authorization header"}},
		{"credential too large", http.MethodPost, func(*http.Request) (map[string]string, error) {
			return nil, restgw.ErrWSAuthTicketHeaderTooLarge
		}, restgw.NewMemoryTicketStore(), http.StatusRequestHeaderFieldsTooLarge, "INVALID_ARGUMENT", []string{"restgw:"}},
		{"store full", http.MethodPost, okPrincipal, failingTicketStore{restgw.ErrTicketStoreFull}, http.StatusServiceUnavailable, "UNAVAILABLE", []string{"restgw:", "store full"}},
		{"store failure", http.MethodPost, okPrincipal, failingTicketStore{errors.New("redis 10.0.0.7:6379: connection reset")}, http.StatusInternalServerError, "INTERNAL", []string{"redis", "10.0.0.7"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{Principal: tc.principal, Store: tc.store})
			rec := httptest.NewRecorder()
			issuer.ServeHTTP(rec, httptest.NewRequest(tc.method, "/api/ws/ticket", nil))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			assertEnvelope(t, rec, tc.code, tc.leaks...)
		})
	}
}

type statFailingDriver struct {
	*memory.Driver
	err error
}

func (d statFailingDriver) Stat(context.Context, string) (kvfs.Info, error) { return kvfs.Info{}, d.err }

func TestServeDownload_ErrorsAreTheEnvelope(t *testing.T) {
	_, cfg := newTestDownload(t)

	t.Run("not found", func(t *testing.T) {
		rec := httptest.NewRecorder()
		restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/api/files/static/avatars/missing", nil), cfg)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		assertEnvelope(t, rec, "NOT_FOUND")
	})
	t.Run("unknown bucket", func(t *testing.T) {
		rec := httptest.NewRecorder()
		restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/api/files/static/secrets/x", nil), cfg)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		assertEnvelope(t, rec, "NOT_FOUND", "secrets")
	})
	t.Run("wrong method", func(t *testing.T) {
		rec := httptest.NewRecorder()
		restgw.ServeDownload(rec, httptest.NewRequest(http.MethodDelete, "/api/files/static/avatars/0123456789abcdef", nil), cfg)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
		if rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("Allow = %q, want GET, HEAD", rec.Header().Get("Allow"))
		}
		assertEnvelope(t, rec, "UNIMPLEMENTED")
	})
	t.Run("driver failure", func(t *testing.T) {
		broken := restgw.NewDownloadConfig("/api/files/static",
			[]restgw.DownloadField{{BucketPath: "/avatars", Driver: statFailingDriver{memory.New(), errors.New("stat /srv/uploads/avatars/x: permission denied")}}},
			"", false, false, false)
		rec := httptest.NewRecorder()
		restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/api/files/static/avatars/x", nil), broken)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		assertEnvelope(t, rec, "INTERNAL", "/srv/uploads", "permission denied")
	})
}
