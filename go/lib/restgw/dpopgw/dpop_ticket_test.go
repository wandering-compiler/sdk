package dpopgw_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
	"github.com/wandering-compiler/sdk/go/lib/restgw/dpopgw"
)

// issueBoundTicket runs the issuer behind the gate, as a DPoP surface mounts
// it, and returns the ticket (or the refusal status).
func issueBoundTicket(t *testing.T, g *dpopgw.Gate, store restgw.TicketStore, r *http.Request) (string, int) {
	t.Helper()
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{Principal: dpopgw.TicketPrincipal, Store: store})
	rec := httptest.NewRecorder()
	g.Middleware(issuer).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		return "", rec.Code
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Ticket, rec.Code
}

type innerSeen struct {
	calls atomic.Int32
	auth  atomic.Value
}

func (s *innerSeen) fn(_ context.Context, r *http.Request) ([]byte, error) {
	s.calls.Add(1)
	s.auth.Store(r.Header.Get("Authorization"))
	return []byte("principal"), nil
}

func upgrade(ticket string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/v1/notes/live?ticket="+ticket, nil)
}

// The bound-ticket flow end to end: the exchange is a verified DPoP request,
// the ticket records its thumbprint, redemption re-checks the binding,
// promotes the token, and reports the device; the ticket is one-shot.
func TestDPoPTicket_BoundFlow(t *testing.T) {
	key := dpKey(t)
	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	g := dpGate(t, dpVerifier(t, nil), b)
	store := restgw.NewMemoryTicketStore()
	defer store.Close()

	ticket, code := issueBoundTicket(t, g, store, dpReq(t, key, http.MethodPost, "/w17-ticket", "tok-1", ""))
	if code != http.StatusOK {
		t.Fatalf("issue: %d", code)
	}

	inner := &innerSeen{}
	a := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{
		WSAuthConfig: restgw.WSAuthConfig{Inner: inner.fn, TicketStore: store, Modes: []string{restgw.WSAuthModeTicket}},
		Gate:         g,
	})
	authFn := a.AuthFunc()
	r := upgrade(ticket)
	if _, err := authFn(r.Context(), r); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if got, _ := inner.auth.Load().(string); got != "DPoP tok-1" {
		t.Errorf("inner saw Authorization %q, want the ticket's DPoP token", got)
	}
	if dev, ok := a.Thumbprint(r); !ok || dev != key.Thumbprint() {
		t.Errorf("Thumbprint = (%q, %v), want %s", dev, ok, key.Thumbprint())
	}
	// A re-auth of the accepted stream does not redeem again.
	if _, err := authFn(r.Context(), r); err != nil {
		t.Errorf("re-auth of an accepted stream: %v", err)
	}
	// One-shot.
	r2 := upgrade(ticket)
	if _, err := authFn(r2.Context(), r2); !errors.Is(err, restgw.ErrTicketInvalid) {
		t.Errorf("second redemption: %v, want ErrTicketInvalid", err)
	}
}

// The issuer issues bound tickets only for a request the gate verified as
// bound: a bearer request, an unbound mint proof, and an issuer mounted
// without the gate are all refused.
func TestDPoPTicket_IssuerNeedsAVerifiedBoundRequest(t *testing.T) {
	key := dpKey(t)
	g := dpGate(t, dpVerifier(t, nil), &bindings{})
	store := restgw.NewMemoryTicketStore()
	defer store.Close()

	bearer := httptest.NewRequest(http.MethodPost, "/w17-ticket", nil)
	bearer.Header.Set("Authorization", "Bearer tok-1")
	if _, code := issueBoundTicket(t, g, store, bearer); code != http.StatusUnauthorized {
		t.Errorf("bearer request: %d, want 401", code)
	}
	mint := dpReq(t, key, http.MethodPost, "/w17-ticket", "", "")
	if _, code := issueBoundTicket(t, g, store, mint); code != http.StatusUnauthorized {
		t.Errorf("unbound proof: %d, want 401", code)
	}

	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{Principal: dpopgw.TicketPrincipal, Store: store})
	rec := httptest.NewRecorder()
	issuer.ServeHTTP(rec, dpReq(t, key, http.MethodPost, "/w17-ticket", "tok-1", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("issuer without the gate: %d, want 401", rec.Code)
	}
}

// Redemption re-checks the binding: a token rebound to another key, revoked,
// or a ticket whose labels were not written by the bound issuer is invalid.
func TestDPoPTicket_RedemptionChecksTheBinding(t *testing.T) {
	key := dpKey(t)
	cases := map[string]func(b *bindings, labels map[string]string){
		"rebound":           func(b *bindings, _ map[string]string) { b.set("tok-1", "other-device") },
		"token now unbound": func(b *bindings, _ map[string]string) { b.set("tok-1", "") },
		"revoked":           func(b *bindings, _ map[string]string) { b.err = status.Error(codes.Unauthenticated, "revoked") },
		"bearer in a bound ticket": func(_ *bindings, l map[string]string) {
			l[restgw.WSAuthTicketHeaderLabel] = "Bearer tok-1"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := &bindings{}
			b.set("tok-1", key.Thumbprint())
			g := dpGate(t, dpVerifier(t, nil), b)
			store := restgw.NewMemoryTicketStore()
			defer store.Close()
			labels := map[string]string{
				restgw.WSAuthTicketHeaderLabel: "DPoP tok-1",
				dpopgw.TicketThumbprintLabel:   key.Thumbprint(),
			}
			mutate(b, labels)
			ticket, err := store.Issue(context.Background(), labels, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			inner := &innerSeen{}
			a := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{
				WSAuthConfig: restgw.WSAuthConfig{Inner: inner.fn, TicketStore: store, Modes: []string{restgw.WSAuthModeTicket}},
				Gate:         g,
			})
			r := upgrade(ticket)
			_, err = a.AuthFunc()(r.Context(), r)
			if !errors.Is(err, restgw.ErrTicketInvalid) {
				t.Fatalf("err = %v, want ErrTicketInvalid", err)
			}
			if inner.calls.Load() != 0 {
				t.Error("inner ran for a ticket whose binding failed")
			}
			if _, ok := a.Thumbprint(r); ok {
				t.Error("a device was reported for a refused ticket")
			}
		})
	}

	// A binding backend that cannot answer is a 503-class error, not a
	// refusal of the ticket.
	b := &bindings{err: status.Error(codes.Unavailable, "down")}
	g := dpGate(t, dpVerifier(t, nil), b)
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	ticket, _ := store.Issue(context.Background(), map[string]string{
		restgw.WSAuthTicketHeaderLabel: "DPoP tok-1", dpopgw.TicketThumbprintLabel: key.Thumbprint(),
	}, time.Minute)
	a := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{
		WSAuthConfig: restgw.WSAuthConfig{Inner: (&innerSeen{}).fn, TicketStore: store, Modes: []string{restgw.WSAuthModeTicket}},
		Gate:         g,
	})
	r := upgrade(ticket)
	_, err := a.AuthFunc()(r.Context(), r)
	if status.Code(err) != codes.Unavailable || errors.Is(err, restgw.ErrTicketInvalid) {
		t.Errorf("binding outage: %v (code %v), want Unavailable", err, status.Code(err))
	}
}

// An unbound ticket (the plain issuer's) is accepted with no device when the
// gate is optional, and refused when it is required.
func TestDPoPTicket_UnboundTicket(t *testing.T) {
	for _, required := range []bool{false, true} {
		var opts []dpopgw.Option
		if required {
			opts = append(opts, dpopgw.WithRequired())
		}
		g := dpGate(t, dpVerifier(t, nil), &bindings{}, opts...)
		store := restgw.NewMemoryTicketStore()
		ticket, _ := store.Issue(context.Background(), map[string]string{restgw.WSAuthTicketHeaderLabel: "Bearer tok-1"}, time.Minute)
		a := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{
			WSAuthConfig: restgw.WSAuthConfig{Inner: (&innerSeen{}).fn, TicketStore: store, Modes: []string{restgw.WSAuthModeTicket}},
			Gate:         g,
		})
		r := upgrade(ticket)
		_, err := a.AuthFunc()(r.Context(), r)
		_, hasDevice := a.Thumbprint(r)
		switch {
		case required && !errors.Is(err, restgw.ErrTicketInvalid):
			t.Errorf("required gate accepted an unbound ticket: %v", err)
		case !required && (err != nil || hasDevice):
			t.Errorf("optional gate: err %v, device %v; want accepted with no device", err, hasDevice)
		}
		store.Close()
	}
}

// Header mode: the upgrade's own proof is verified by the gate (no
// middleware needed), a bad one is a 401-class DPoPError, and a client can
// never make Thumbprint report a device it did not prove.
func TestDPoPWSAuth_HeaderModeAndForgery(t *testing.T) {
	key := dpKey(t)
	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	g := dpGate(t, dpVerifier(t, nil), b)
	inner := &innerSeen{}
	a := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{
		WSAuthConfig: restgw.WSAuthConfig{Inner: inner.fn},
		Gate:         g,
	})
	authFn := a.AuthFunc()

	r := dpReq(t, key, http.MethodGet, "/v1/notes/live", "tok-1", "")
	if _, err := authFn(r.Context(), r); err != nil {
		t.Fatalf("verified upgrade: %v", err)
	}
	if dev, ok := a.Thumbprint(r); !ok || dev != key.Thumbprint() {
		t.Errorf("Thumbprint = (%q, %v)", dev, ok)
	}

	bad := dpReq(t, dpKey(t), http.MethodGet, "/v1/notes/live", "tok-1", "")
	_, err := authFn(bad.Context(), bad)
	var de *dpopgw.Error
	if !errors.As(err, &de) || status.Code(err) != codes.Unauthenticated || !errors.Is(err, dpop.ErrBindingMismatch) {
		t.Errorf("wrong key: %v, want a 401-class DPoPError for a binding mismatch", err)
	}

	forged := httptest.NewRequest(http.MethodGet, "/v1/notes/live", nil)
	forged.Header.Set("Authorization", "Bearer tok-1")
	forged.Header.Set("X-W17-Dpop-Jkt", key.Thumbprint())
	forged.Header.Set("X-W17-Ticket-Promoted", "guess")
	if _, err := authFn(forged.Context(), forged); err != nil {
		t.Fatalf("bearer upgrade: %v", err)
	}
	if dev, ok := a.Thumbprint(forged); ok {
		t.Errorf("a client's own headers made Thumbprint report %q", dev)
	}
	if forged.Header.Get("X-W17-Dpop-Jkt") != "" {
		t.Error("the client's thumbprint header reached Inner")
	}

	// Another instance's marker is not this one's.
	other := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{WSAuthConfig: restgw.WSAuthConfig{Inner: inner.fn}, Gate: g})
	if _, ok := other.Thumbprint(r); ok {
		t.Error("one wrapper trusted another wrapper's promotion")
	}
}

func TestNewDPoPWSAuth_PanicsWithoutGate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic without a gate")
		}
	}()
	dpopgw.NewWSAuth(dpopgw.WSAuthConfig{WSAuthConfig: restgw.WSAuthConfig{Inner: (&innerSeen{}).fn}})
}

// --- binding watchdog ---

func boundStreamReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/notes/live", nil)
	if token != "" {
		r.Header.Set("Authorization", "DPoP "+token)
	}
	return r
}

// The binding watchdog asks the binding question with the stream's token
// and thumbprint, never the auth function, and ends the stream on a refusal.
func TestWatchStreamBinding_AsksTheBindingQuestion(t *testing.T) {
	var calls atomic.Int32
	var gotToken, gotThumb atomic.Value
	check := func(_ context.Context, token, thumb string) error {
		gotToken.Store(token)
		gotThumb.Store(thumb)
		if calls.Add(1) >= 3 {
			return status.Error(codes.Unauthenticated, "device revoked")
		}
		return nil
	}
	ctx, stop := dpopgw.WatchStreamBinding(context.Background(), check, boundStreamReq("tok-1"), "jkt-1", 5*time.Millisecond)
	defer stop()
	if !waitCancelled(ctx, 2*time.Second) {
		t.Fatal("a revoked device kept streaming")
	}
	if gotToken.Load() != "tok-1" || gotThumb.Load() != "jkt-1" {
		t.Errorf("check got (%v, %v), want (tok-1, jkt-1)", gotToken.Load(), gotThumb.Load())
	}
}

// Every definite refusal ends the stream; an outage keeps it open and
// re-asks, as the auth watchdog does.
func TestWatchStreamBinding_RefusedVersusUnanswered(t *testing.T) {
	for name, refusal := range map[string]error{
		"unauthenticated":   status.Error(codes.Unauthenticated, "revoked"),
		"permission denied": status.Error(codes.PermissionDenied, "device revoked"),
		"binding mismatch":  dpop.ErrBindingMismatch,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, stop := dpopgw.WatchStreamBinding(context.Background(),
				func(context.Context, string, string) error { return refusal }, boundStreamReq("tok-1"), "jkt-1", 5*time.Millisecond)
			defer stop()
			if !waitCancelled(ctx, 2*time.Second) {
				t.Fatalf("%v did not end the stream", refusal)
			}
		})
	}
	var calls atomic.Int32
	ctx, stop := dpopgw.WatchStreamBinding(context.Background(), func(context.Context, string, string) error {
		calls.Add(1)
		return status.Error(codes.Unavailable, "down")
	}, boundStreamReq("tok-1"), "jkt-1", 5*time.Millisecond)
	defer stop()
	if waitCancelled(ctx, 150*time.Millisecond) {
		t.Fatal("an outage ended the stream")
	}
	if calls.Load() < 3 {
		t.Errorf("probed %d times during the outage", calls.Load())
	}
}

// A stream with no binding to check ends at once; a disabled watchdog
// changes nothing.
func TestWatchStreamBinding_NoBindingEndsTheStream(t *testing.T) {
	never := func(context.Context, string, string) error { t.Error("check called"); return nil }
	for name, tc := range map[string]struct {
		r     *http.Request
		thumb string
	}{
		"no token":         {boundStreamReq(""), "jkt-1"},
		"bearer token":     {func() *http.Request { r := boundStreamReq(""); r.Header.Set("Authorization", "Bearer x"); return r }(), "jkt-1"},
		"empty thumbprint": {boundStreamReq("tok-1"), ""},
	} {
		ctx, stop := dpopgw.WatchStreamBinding(context.Background(), never, tc.r, tc.thumb, time.Millisecond)
		if ctx.Err() == nil {
			t.Errorf("%s: stream left open with nothing to check", name)
		}
		stop()
	}
	base := context.Background()
	if ctx, stop := dpopgw.WatchStreamBinding(base, never, boundStreamReq("tok-1"), "jkt-1", 0); ctx != base {
		t.Error("interval 0 did not return ctx unchanged")
	} else {
		stop()
	}
	if ctx, stop := dpopgw.WatchStreamBinding(base, nil, boundStreamReq("tok-1"), "jkt-1", time.Second); ctx != base {
		t.Error("nil check did not return ctx unchanged")
	} else {
		stop()
	}
}

// A stream Inner refuses is never marked: Thumbprint reports no device and a
// second pass over the same request verifies again instead of trusting it.
func TestDPoPWSAuth_RefusedByInnerIsNotMarked(t *testing.T) {
	key := dpKey(t)
	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	g := dpGate(t, dpVerifier(t, nil), b)
	refuse := func(context.Context, *http.Request) ([]byte, error) {
		return nil, status.Error(codes.Unauthenticated, "session revoked")
	}
	a := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{WSAuthConfig: restgw.WSAuthConfig{Inner: refuse}, Gate: g})
	r := dpReq(t, key, http.MethodGet, "/v1/notes/live", "tok-1", "")
	if _, err := a.AuthFunc()(r.Context(), r); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Inner's refusal", err)
	}
	if dev, ok := a.Thumbprint(r); ok {
		t.Errorf("a refused stream reports device %q", dev)
	}

	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	ticket, _ := store.Issue(context.Background(), map[string]string{
		restgw.WSAuthTicketHeaderLabel: "DPoP tok-1", dpopgw.TicketThumbprintLabel: key.Thumbprint(),
	}, time.Minute)
	at := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{
		WSAuthConfig: restgw.WSAuthConfig{Inner: refuse, TicketStore: store, Modes: []string{restgw.WSAuthModeTicket}},
		Gate:         g,
	})
	tr := upgrade(ticket)
	if _, err := at.AuthFunc()(tr.Context(), tr); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ticket: err = %v, want Inner's refusal", err)
	}
	if _, ok := at.Thumbprint(tr); ok {
		t.Error("a refused ticket stream reports a device")
	}
	if _, err := at.AuthFunc()(tr.Context(), tr); !errors.Is(err, restgw.ErrTicketInvalid) {
		t.Errorf("second pass over a refused ticket stream: %v, want the spent ticket refused", err)
	}
}

// The WS header path marks a stream only for a token-bound proof: a cookie
// upgrade with a fresh key's unbound proof is accepted (Inner decides) but
// reports no device, and Verified is false.
func TestDPoPWSAuth_UnboundHeaderProofIsNoDevice(t *testing.T) {
	g := dpGate(t, dpVerifier(t, nil), &bindings{})
	inner := &innerSeen{}
	a := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{WSAuthConfig: restgw.WSAuthConfig{Inner: inner.fn}, Gate: g})
	r := dpReq(t, dpKey(t), http.MethodGet, "/v1/notes/live", "", "")
	r.Header.Set("Cookie", "session=s1")
	if _, err := a.AuthFunc()(r.Context(), r); err != nil {
		t.Fatalf("cookie upgrade: %v", err)
	}
	if dev, ok := a.Thumbprint(r); ok {
		t.Errorf("an unbound proof marked the stream with device %q", dev)
	}
	if _, ok := a.Verified(r); ok {
		t.Error("Verified reports a device for an unbound proof")
	}

	// A bound upgrade reports the device and the token for SetDeviceMetadata.
	key := dpKey(t)
	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	ab := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{WSAuthConfig: restgw.WSAuthConfig{Inner: inner.fn}, Gate: dpGate(t, dpVerifier(t, nil), b)})
	br := dpReq(t, key, http.MethodGet, "/v1/notes/live", "tok-1", "")
	if _, err := ab.AuthFunc()(br.Context(), br); err != nil {
		t.Fatal(err)
	}
	if v, ok := ab.Verified(br); !ok || v.DeviceID() != key.Thumbprint() || v.AccessToken != "tok-1" {
		t.Errorf("Verified = (%+v, %v)", v, ok)
	}
}

// An unbound ticket (no thumbprint label) holding a DPoP token did not come
// from the bound issuer; replaying it unbound would skip its binding, so it
// is refused even when the gate is optional.
func TestDPoPTicket_UnboundTicketWithDPoPTokenRefused(t *testing.T) {
	g := dpGate(t, dpVerifier(t, nil), &bindings{})
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	inner := &innerSeen{}
	a := dpopgw.NewWSAuth(dpopgw.WSAuthConfig{
		WSAuthConfig: restgw.WSAuthConfig{Inner: inner.fn, TicketStore: store, Modes: []string{restgw.WSAuthModeTicket}},
		Gate:         g,
	})
	for _, auth := range []string{"DPoP tok-1", "dpop tok-1", "DPoP"} {
		ticket, _ := store.Issue(context.Background(), map[string]string{restgw.WSAuthTicketHeaderLabel: auth}, time.Minute)
		r := upgrade(ticket)
		if _, err := a.AuthFunc()(r.Context(), r); !errors.Is(err, restgw.ErrTicketInvalid) {
			t.Errorf("unbound ticket with %q: %v, want ErrTicketInvalid", auth, err)
		}
	}
	if inner.calls.Load() != 0 {
		t.Error("inner ran for an unbound ticket carrying a DPoP token")
	}
}

// waitCancelled reports whether ctx is done within the deadline.
func waitCancelled(ctx context.Context, within time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(within):
		return false
	}
}
