package restgw_test

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// promotedHeader is the header the ticket wrapper marks a promoted
// credential with. Its name is public knowledge; its value is the secret.
const promotedHeader = "X-W17-Ticket-Promoted"

func ticketOnly(t *testing.T, store restgw.TicketStore, calls *int) restgw.AuthFunc {
	t.Helper()
	return restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner: func(_ context.Context, r *http.Request) ([]byte, error) {
			*calls++
			return []byte(r.Header.Get("Authorization")), nil
		},
		TicketStore: store,
		Modes:       []string{restgw.WSAuthModeTicket},
	})
}

// In ticket-only mode a bearer is accepted only as the promotion of a
// redeemed ticket. A client that sends the marker header itself, with any
// value it can come up with, must still need a ticket.
func TestWSAuth_TicketOnly_ClientCannotPresentThePromotionMarker(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	calls := 0
	wrapped := ticketOnly(t, store, &calls)

	for _, guess := range []string{
		"",
		"1",
		"true",
		strings.Repeat("0", 64),
		strings.Repeat("f", 64),
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer forged")
		r.Header.Set(promotedHeader, guess)
		if _, err := wrapped(context.Background(), r); !errors.Is(err, restgw.ErrWSAuthNoTicket) {
			t.Errorf("marker %q: err = %v, want ErrWSAuthNoTicket", guess, err)
		}
	}
	if calls != 0 {
		t.Fatalf("Inner ran %d times for requests that carried no ticket", calls)
	}
}

// The marker is per wrapper instance: one replica's (or one route's)
// marker means nothing to another, so a marker that did escape would not
// open any other instance.
func TestWSAuth_PromotionMarkerIsPerInstance(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	callsA, callsB := 0, 0
	a := ticketOnly(t, store, &callsA)
	b := ticketOnly(t, store, &callsB)

	ticket, err := store.Issue(context.Background(), map[string]string{"authorization": "Bearer real"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/?ticket="+ticket, nil)
	if _, err := a(context.Background(), r); err != nil {
		t.Fatalf("redeem on A: %v", err)
	}
	marker := r.Header.Get(promotedHeader)
	if raw, err := hex.DecodeString(marker); err != nil || len(raw) != 32 {
		t.Fatalf("marker %q is not 256 bits of hex", marker)
	}

	// A honours its own marker on the stream's re-auth...
	if _, err := a(context.Background(), r); err != nil {
		t.Fatalf("re-auth on A with its own marker: %v", err)
	}
	// ...B does not.
	replay := httptest.NewRequest(http.MethodGet, "/", nil)
	replay.Header.Set("Authorization", "Bearer real")
	replay.Header.Set(promotedHeader, marker)
	if _, err := b(context.Background(), replay); !errors.Is(err, restgw.ErrWSAuthNoTicket) {
		t.Fatalf("B accepted A's marker: err = %v", err)
	}
	if callsB != 0 {
		t.Fatalf("B's Inner ran %d times", callsB)
	}
}
