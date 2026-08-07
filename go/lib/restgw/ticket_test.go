package restgw_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// G3i3-GW-F: Issue + Redeem round-trip — happy path returns
// the same labels the caller stored.
func TestTicket_IssueRedeem_RoundTrip(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()

	labels := map[string]string{"authorization": "Bearer abc"}
	ticket, err := store.Issue(context.Background(), labels, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if ticket == "" {
		t.Fatal("empty ticket")
	}
	got, err := store.Redeem(context.Background(), ticket)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got["authorization"] != "Bearer abc" {
		t.Errorf("labels round-trip lost: %v", got)
	}
}

// G3i3-GW-F: tickets are one-shot — second Redeem of the
// same ticket fails. Critical security property: a leaked
// ticket can't be replayed.
func TestTicket_OneShot(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()

	ticket, _ := store.Issue(context.Background(), map[string]string{"k": "v"}, time.Minute)
	if _, err := store.Redeem(context.Background(), ticket); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if _, err := store.Redeem(context.Background(), ticket); err == nil {
		t.Fatal("second Redeem should fail (one-shot)")
	}
}

// G3i3-GW-F: unknown ticket → ErrTicketInvalid (same as
// expired / consumed — attacker shouldn't probe validity).
func TestTicket_UnknownInvalid(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	_, err := store.Redeem(context.Background(), "not-a-real-ticket")
	if err == nil {
		t.Fatal("expected error on unknown ticket")
	}
}

// G3i3-GW-F: empty ticket string also returns ErrTicketInvalid
// without hitting the store map.
func TestTicket_EmptyInvalid(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	if _, err := store.Redeem(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty ticket")
	}
}

// G3i3-GW-F: TTL expiry — past-TTL Redeem fails. Use a tiny
// TTL + sleep instead of stubbing time so the real clock-
// driven path runs.
func TestTicket_TTLExpiry(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	ticket, _ := store.Issue(context.Background(), nil, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if _, err := store.Redeem(context.Background(), ticket); err == nil {
		t.Fatal("expected expiry error")
	}
}

// G3i3-GW-F: NewTicketIssuer happy path — POST with valid
// Principal returns 200 + JSON body containing ticket +
// expires_in. Vary + Cache-Control: no-store on the response.
func TestTicketIssuer_HappyPOST(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
		Principal: func(*http.Request) (map[string]string, error) {
			return map[string]string{"authorization": "Bearer t"}, nil
		},
		Store: store,
		TTL:   30 * time.Second,
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
	r.Header.Set("Authorization", "Bearer t")
	issuer.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Vary") == "" {
		t.Errorf("Vary missing")
	}

	var body struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Ticket == "" {
		t.Error("ticket empty")
	}
	if body.ExpiresIn != 30 {
		t.Errorf("expires_in = %d, want 30", body.ExpiresIn)
	}
}

// G3i3-GW-F: non-POST → 405 with Allow header. Pin the
// method-allowlist semantic so a future router rewrite
// doesn't quietly accept GET (which would let a malicious
// page issue tickets via <img> with the user's cookie).
func TestTicketIssuer_RejectsGET(t *testing.T) {
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
		Principal: func(*http.Request) (map[string]string, error) {
			return nil, nil
		},
		Store: restgw.NewMemoryTicketStore(),
	})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/ws/ticket", nil)
	issuer.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}
}

// G3i3-GW-F: Principal failure → 401 (not 500 — the failure
// IS the auth result for the issuer).
func TestTicketIssuer_PrincipalFailure_401(t *testing.T) {
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
		Principal: func(*http.Request) (map[string]string, error) {
			return nil, restgw.ErrWSAuthTicketNoHeader
		},
		Store: restgw.NewMemoryTicketStore(),
	})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
	issuer.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
