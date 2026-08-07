package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// G3i3-GW-F: header-only mode (default) forwards
// Authorization-bearing requests to Inner unchanged. No
// ticket lookup attempted.
func TestWSAuth_HeaderOnly_Passthrough(t *testing.T) {
	called := false
	inner := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		called = true
		if r.Header.Get("Authorization") != "Bearer abc" {
			return nil, errors.New("missing header")
		}
		return []byte("ok"), nil
	})
	wrapped := restgw.NewWSAuth(restgw.WSAuthConfig{Inner: inner})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer abc")
	data, err := wrapped(context.Background(), r)
	if err != nil || string(data) != "ok" || !called {
		t.Errorf("got (%q, %v); inner-called=%v", data, err, called)
	}
}

// G3i3-GW-F: ticket mode — query carries ticket, store
// redeems, recorded Authorization replays into a CLONE of
// the request (caller's r.Header NOT mutated).
func TestWSAuth_TicketMode_Replays(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	ticket, _ := store.Issue(context.Background(),
		map[string]string{"authorization": "Bearer xyz"}, time.Minute)

	var seenAuth string
	inner := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		seenAuth = r.Header.Get("Authorization")
		return []byte("ok"), nil
	})
	wrapped := restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner:       inner,
		TicketStore: store,
		Modes:       []string{restgw.WSAuthModeTicket},
	})

	r := httptest.NewRequest(http.MethodGet, "/?ticket="+ticket, nil)
	if _, err := wrapped(context.Background(), r); err != nil {
		t.Fatalf("WSAuth: %v", err)
	}
	if seenAuth != "Bearer xyz" {
		t.Errorf("inner saw Authorization = %q, want replayed Bearer xyz", seenAuth)
	}
	// Caller's request header MUST be untouched (clone semantic).
	if r.Header.Get("Authorization") != "" {
		t.Errorf("caller's request header was mutated: %q", r.Header.Get("Authorization"))
	}
}

// G3i3-GW-F: ticket-only + missing ticket → ErrWSAuthNoTicket.
// Critical: must NOT forward to Inner with a stray
// Authorization header attached.
func TestWSAuth_TicketOnly_NoTicket_FailsFast(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	called := false
	inner := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		called = true
		return nil, nil
	})
	wrapped := restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner:       inner,
		TicketStore: store,
		Modes:       []string{restgw.WSAuthModeTicket},
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// Stray header — must not leak through.
	r.Header.Set("Authorization", "Bearer attacker")
	_, err := wrapped(context.Background(), r)
	if !errors.Is(err, restgw.ErrWSAuthNoTicket) {
		t.Errorf("err = %v, want ErrWSAuthNoTicket", err)
	}
	if called {
		t.Error("Inner must NOT be called in ticket-only mode without ticket")
	}
}

// G3i3-GW-F: header+ticket combo — Authorization wins when
// set, ticket ignored.
func TestWSAuth_HeaderAndTicket_HeaderWins(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	ticket, _ := store.Issue(context.Background(),
		map[string]string{"authorization": "Bearer ticket-replay"}, time.Minute)

	var seenAuth string
	inner := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		seenAuth = r.Header.Get("Authorization")
		return []byte("ok"), nil
	})
	wrapped := restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner:       inner,
		TicketStore: store,
		Modes:       []string{restgw.WSAuthModeHeader, restgw.WSAuthModeTicket},
	})

	r := httptest.NewRequest(http.MethodGet, "/?ticket="+ticket, nil)
	r.Header.Set("Authorization", "Bearer header-direct")
	if _, err := wrapped(context.Background(), r); err != nil {
		t.Fatalf("WSAuth: %v", err)
	}
	if seenAuth != "Bearer header-direct" {
		t.Errorf("seenAuth = %q, want Authorization to win over ticket", seenAuth)
	}
	// Ticket NOT consumed (we took the header path) — verify
	// by attempting Redeem and checking it still works.
	if _, err := store.Redeem(context.Background(), ticket); err != nil {
		t.Errorf("ticket should still be redeemable when header path was taken; got %v", err)
	}
}

// G3i3-GW-F: header+ticket combo, neither present → forward
// to Inner unchanged so Inner's missing-credentials branch
// produces the 401.
func TestWSAuth_HeaderAndTicket_NeitherForwards(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	innerErr := errors.New("missing creds")
	called := false
	inner := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		called = true
		return nil, innerErr
	})
	wrapped := restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner:       inner,
		TicketStore: store,
		Modes:       []string{restgw.WSAuthModeHeader, restgw.WSAuthModeTicket},
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := wrapped(context.Background(), r)
	if !called {
		t.Error("Inner should be called when neither header nor ticket present (header+ticket mode)")
	}
	if !errors.Is(err, innerErr) {
		t.Errorf("err = %v, want innerErr", err)
	}
}

// G3i3-GW-F: invalid ticket → ErrTicketInvalid surfaces.
func TestWSAuth_TicketMode_InvalidTicket(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	inner := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		return nil, nil
	})
	wrapped := restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner:       inner,
		TicketStore: store,
		Modes:       []string{restgw.WSAuthModeTicket},
	})

	r := httptest.NewRequest(http.MethodGet, "/?ticket=bogus", nil)
	_, err := wrapped(context.Background(), r)
	if !errors.Is(err, restgw.ErrTicketInvalid) {
		t.Errorf("err = %v, want ErrTicketInvalid", err)
	}
}

// G3i3-GW-F: WSAuthTicketPrincipal records Authorization;
// missing → ErrWSAuthTicketNoHeader.
func TestWSAuthTicketPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
	r.Header.Set("Authorization", "Bearer abc")
	labels, err := restgw.WSAuthTicketPrincipal(r)
	if err != nil {
		t.Fatalf("with header: %v", err)
	}
	if labels[restgw.WSAuthTicketHeaderLabel] != "Bearer abc" {
		t.Errorf("labels[%q] = %q, want Bearer abc",
			restgw.WSAuthTicketHeaderLabel, labels[restgw.WSAuthTicketHeaderLabel])
	}

	r2 := httptest.NewRequest(http.MethodPost, "/api/ws/ticket", nil)
	if _, err := restgw.WSAuthTicketPrincipal(r2); !errors.Is(err, restgw.ErrWSAuthTicketNoHeader) {
		t.Errorf("missing header err = %v, want ErrWSAuthTicketNoHeader", err)
	}
}

// G3i3-GW-F: nil Inner panics at NewWSAuth — startup-time
// failure beats first-upgrade silent breakage.
func TestNewWSAuth_NilInnerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for nil Inner")
		}
	}()
	_ = restgw.NewWSAuth(restgw.WSAuthConfig{})
}

// G3i3-GW-F: ticket mode without a TicketStore panics.
func TestNewWSAuth_TicketWithoutStorePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for ticket mode without store")
		}
	}()
	inner := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) { return nil, nil })
	_ = restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner: inner,
		Modes: []string{restgw.WSAuthModeTicket},
	})
}

// G3i3-GW-F: unknown Modes value panics.
func TestNewWSAuth_UnknownModePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for unknown Modes value")
		}
	}()
	inner := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) { return nil, nil })
	_ = restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner: inner,
		Modes: []string{"bearer-but-typo"},
	})
}

// TicketLabelsMissingAuthorization: a redeemed ticket whose labels lack
// the authorization entry is an error (the replay can't reconstruct the
// header). Pins the auth=="" arm.
func TestWSAuth_TicketLabelsMissingAuthorization(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	ticket, _ := store.Issue(context.Background(), map[string]string{"other": "x"}, time.Minute)

	inner := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) { return []byte("ok"), nil })
	wrapped := restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner:       inner,
		TicketStore: store,
		Modes:       []string{restgw.WSAuthModeTicket},
	})
	r := httptest.NewRequest(http.MethodGet, "/?ticket="+ticket, nil)
	if _, err := wrapped(context.Background(), r); err == nil {
		t.Error("ticket without an authorization label must error")
	}
}

// HeaderOnly_NoAuth_ForwardsToInner: header-only mode with no Authorization
// header falls through to Inner (which issues the standard 401). Pins the
// final fall-through return.
func TestWSAuth_HeaderOnly_NoAuth_ForwardsToInner(t *testing.T) {
	called := false
	inner := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		called = true
		return nil, errors.New("missing credentials")
	})
	wrapped := restgw.NewWSAuth(restgw.WSAuthConfig{Inner: inner})
	r := httptest.NewRequest(http.MethodGet, "/", nil) // no Authorization
	if _, err := wrapped(context.Background(), r); err == nil {
		t.Error("expected Inner's missing-credentials error")
	}
	if !called {
		t.Error("header-only + no auth must fall through to Inner")
	}
}
