package restgw_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// Issue + Redeem round-trip — happy path returns
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

// Tickets are one-shot — second Redeem of the
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

// Unknown ticket → ErrTicketInvalid (same as
// expired / consumed — attacker shouldn't probe validity).
func TestTicket_UnknownInvalid(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	_, err := store.Redeem(context.Background(), "not-a-real-ticket")
	if err == nil {
		t.Fatal("expected error on unknown ticket")
	}
}

// Empty ticket string also returns ErrTicketInvalid
// without hitting the store map.
func TestTicket_EmptyInvalid(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	if _, err := store.Redeem(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty ticket")
	}
}

// TTL expiry — past-TTL Redeem fails. Use a tiny
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

// NewTicketIssuer happy path — POST with valid
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
	// The ticket depends on who asked: a cache keyed on anything less
	// would hand one caller's ticket to another.
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Authorization") || !strings.Contains(vary, "Cookie") {
		t.Errorf("Vary = %q, want it to name Authorization and Cookie", vary)
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

// Non-POST → 405 with Allow header. Pin the
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

// Principal failure → 401 (not 500 — the failure
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

// The default store must not grow without bound: the issuer mints for any
// request carrying a credential, before the credential is validated.
func TestTicketIssuer_FloodIsBounded(t *testing.T) {
	store := restgw.NewMemoryTicketStore(restgw.WithMaxTickets(50))
	defer store.Close()
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
		Principal: restgw.WSAuthTicketPrincipal,
		Store:     store,
	})
	issued, refused := 0, 0
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
		r.Header.Set("Authorization", "Bearer not-validated")
		// Many addresses: one address alone stops at its own cap
		// (TestTicketIssuer_OneAddressCannotFillTheStore).
		r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", i)
		issuer.ServeHTTP(rec, r)
		switch rec.Code {
		case http.StatusOK:
			issued++
		case http.StatusServiceUnavailable:
			refused++
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("503 without Retry-After")
			}
		default:
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if issued != 50 || refused != 150 {
		t.Errorf("issued=%d refused=%d, want 50/150", issued, refused)
	}
}

// A redeemed ticket frees its slot.
func TestTicketStore_RedeemFreesCapacity(t *testing.T) {
	store := restgw.NewMemoryTicketStore(restgw.WithMaxTickets(1))
	defer store.Close()
	ctx := context.Background()
	tk, err := store.Issue(ctx, map[string]string{"a": "b"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := store.Issue(ctx, nil, time.Minute); !errors.Is(err, restgw.ErrTicketStoreFull) {
		t.Fatalf("second Issue err = %v, want ErrTicketStoreFull", err)
	}
	if _, err := store.Redeem(ctx, tk); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if _, err := store.Issue(ctx, nil, time.Minute); err != nil {
		t.Fatalf("Issue after redeem: %v", err)
	}
}

// The byte budget counts label payload, so few large tickets fill it.
func TestTicketStore_ByteBudget(t *testing.T) {
	store := restgw.NewMemoryTicketStore(restgw.WithMaxTicketBytes(4096))
	defer store.Close()
	ctx := context.Background()
	big := map[string]string{"authorization": strings.Repeat("x", 3000)}
	if _, err := store.Issue(ctx, big, time.Minute); err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	if _, err := store.Issue(ctx, big, time.Minute); !errors.Is(err, restgw.ErrTicketStoreFull) {
		t.Fatalf("second Issue err = %v, want ErrTicketStoreFull", err)
	}
}

// WSAuthTicketPrincipal refuses an oversized credential and the issuer maps
// it to 431, storing nothing.
func TestTicketIssuer_OversizedAuthorization(t *testing.T) {
	store := restgw.NewMemoryTicketStore(restgw.WithMaxTickets(1))
	defer store.Close()
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
		Principal: restgw.WSAuthTicketPrincipal,
		Store:     store,
	})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", restgw.MaxTicketAuthorizationBytes))
	issuer.ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431", rec.Code)
	}
	// The single slot is still free.
	if _, err := store.Issue(context.Background(), nil, time.Minute); err != nil {
		t.Fatalf("Issue after refused oversized request: %v", err)
	}

	// At the limit is still accepted.
	ok := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
	ok.Header.Set("Authorization", strings.Repeat("x", restgw.MaxTicketAuthorizationBytes))
	if _, err := restgw.WSAuthTicketPrincipal(ok); err != nil {
		t.Fatalf("Authorization at the limit refused: %v", err)
	}
}

// One address MUST NOT be able to fill the store: the issuer mints before the
// credential is checked, so without a per-address cap one unauthenticated
// client flooding the issuer would leave every other browser on the replica
// without a ticket (503) for as long as it kept going. Each address holds at
// most DefaultMaxTicketsPerAddress outstanding tickets; the next is refused
// with 429 and Retry-After, another address is unaffected, and a redeemed
// ticket gives the address its slot back.
func TestTicketIssuer_OneAddressCannotFillTheStore(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
		Principal: restgw.WSAuthTicketPrincipal,
		Store:     store,
	})
	post := func(addr, auth string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
		r.Header.Set("Authorization", auth)
		r.RemoteAddr = addr
		issuer.ServeHTTP(rec, r)
		return rec
	}
	var first string
	issued, limited := 0, 0
	for i := 0; i < restgw.DefaultMaxTickets+5; i++ {
		rec := post("203.0.113.66:1234", "x")
		switch rec.Code {
		case http.StatusOK:
			issued++
			if first == "" {
				var body struct{ Ticket string }
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				first = body.Ticket
			}
		case http.StatusTooManyRequests:
			limited++
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
		default:
			t.Fatalf("flood request %d: status %d", i, rec.Code)
		}
	}
	if issued != restgw.DefaultMaxTicketsPerAddress {
		t.Fatalf("one address got %d tickets, want its cap %d", issued, restgw.DefaultMaxTicketsPerAddress)
	}
	if rec := post("198.51.100.7:5555", "Bearer a-real-session"); rec.Code != http.StatusOK {
		t.Fatalf("another address after the flood: %d %s, want 200", rec.Code, rec.Body)
	}
	if _, err := store.Redeem(context.Background(), first); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if rec := post("203.0.113.66:1234", "x"); rec.Code != http.StatusOK {
		t.Fatalf("after a redeem the flooding address got %d, want its freed slot", rec.Code)
	}
	if rec := post("203.0.113.66:1234", "x"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the freed slot was given twice: %d", rec.Code)
	}
}

// Behind a trusted proxy the issuer counts the address ClientAddrMiddleware
// resolved, not the proxy's: clients behind one terminator do not share a
// cap, and a client cannot pick its own address by sending X-Forwarded-For to
// a peer that is not trusted.
func TestTicketIssuer_PerAddressCapUsesTheResolvedClient(t *testing.T) {
	store := restgw.NewMemoryTicketStore(restgw.WithMaxTicketsPerAddress(1))
	defer store.Close()
	proxy := netip.MustParsePrefix("10.0.0.0/8")
	h := restgw.ClientAddrMiddleware([]netip.Prefix{proxy})(restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
		Principal: restgw.WSAuthTicketPrincipal,
		Store:     store,
	}))
	post := func(peer, xff string) int {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
		r.Header.Set("Authorization", "x")
		r.RemoteAddr = peer
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if c := post("10.1.1.1:1", "203.0.113.1"); c != http.StatusOK {
		t.Fatalf("client A: %d", c)
	}
	if c := post("10.1.1.1:1", "203.0.113.2"); c != http.StatusOK {
		t.Fatalf("client B behind the same proxy shares A's cap: %d", c)
	}
	if c := post("10.1.1.1:1", "203.0.113.1"); c != http.StatusTooManyRequests {
		t.Fatalf("client A's second ticket: %d, want 429", c)
	}
	if c := post("192.0.2.9:1", "203.0.113.50"); c != http.StatusOK {
		t.Fatalf("untrusted peer: %d", c)
	}
	if c := post("192.0.2.9:1", "203.0.113.51"); c != http.StatusTooManyRequests {
		t.Fatalf("an untrusted peer chose a new address by X-Forwarded-For: %d", c)
	}
}
