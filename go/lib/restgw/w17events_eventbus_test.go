package restgw_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// rawTranscode is the test stub that returns the envelope
// bytes verbatim as the JSON payload — collapses the
// transcode step into a no-op so tests can drive the adapter
// without a real per-domain envelope schema.
func rawTranscode(topic string, envelope []byte) ([]byte, map[string]string, bool, error) {
	return envelope, nil, false, nil
}

// TestEventbusEventSource_ForwardsMatchingTopic — events
// published on subscribed topics land as SSE frames;
// non-subscribed topics drop server-side without leaving
// the bus.
func TestEventbusEventSource_ForwardsMatchingTopic(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, rawTranscode, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=user.created,task.completed", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(rec, req, source, 1*time.Hour)
		close(done)
	}()

	// Give the handler a beat to wire up the subscription.
	time.Sleep(20 * time.Millisecond)

	// Publish — two matching topics + one drop.
	if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String(`{"id":"u-1"}`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := bus.Dispatch(context.Background(), "default", "task.completed", wrapperspb.String(`{"id":"t-7"}`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := bus.Dispatch(context.Background(), "default", "billing.invoice.issued", wrapperspb.String(`{"id":"i-9"}`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Let everything drain + ctx fire.
	<-done

	body := rec.Body.String()
	for _, want := range []string{
		"event: user.created\n",
		"event: task.completed\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if strings.Contains(body, "billing.invoice.issued") {
		t.Errorf("non-subscribed topic leaked into body:\n%s", body)
	}
}

// TestEventbusEventSource_MultiChannelComposesSources — a hub
// configured with several channels opens ONE bus subscriber per
// channel and composes them into a single SSE surface: an event
// on ANY of the domain's channels reaches the client (T2-6 pass
// #2 multi-channel hub).
func TestEventbusEventSource_MultiChannelComposesSources(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	cf := &countingFactory{inner: bus}
	source := restgw.NewEventbusEventSource(cf, []string{"audit", "tasks"}, rawTranscode, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=task.created,user.audited", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(rec, req, source, 1*time.Hour)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)

	// One bus subscriber per configured channel.
	if got := cf.count.Load(); got != 2 {
		t.Fatalf("expected one bus subscriber per channel (2); got %d", got)
	}

	// An event on EACH channel reaches the single SSE client.
	if err := bus.Dispatch(context.Background(), "tasks", "task.created", wrapperspb.String(`{"id":"t-1"}`)); err != nil {
		t.Fatalf("dispatch tasks: %v", err)
	}
	if err := bus.Dispatch(context.Background(), "audit", "user.audited", wrapperspb.String(`{"id":"u-1"}`)); err != nil {
		t.Fatalf("dispatch audit: %v", err)
	}
	<-done

	body := rec.Body.String()
	for _, want := range []string{"event: task.created\n", "event: user.audited\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// countingFactory wraps a SubscriberFactory and counts how many
// times Subscriber() is called — the assertion vehicle for the
// shared-hub property (one bus subscription per channel regardless
// of client count).
type countingFactory struct {
	inner eventbus.SubscriberFactory
	count atomic.Int32
}

func (f *countingFactory) Subscriber(ctx context.Context, channel string) (eventbus.Subscriber, error) {
	f.count.Add(1)
	return f.inner.Subscriber(ctx, channel)
}

// TestEventbusEventSource_SharedHubSingleSubscriber — N
// concurrent SSE clients share ONE bus subscriber (not one
// each), and a single published event fans out to all of them.
// This is the shared-fan-out-hub guarantee: the per-channel
// Subscribe count stays at one however many browsers connect.
func TestEventbusEventSource_SharedHubSingleSubscriber(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	cf := &countingFactory{inner: bus}
	source := restgw.NewEventbusEventSource(cf, []string{"default"}, rawTranscode, nil)

	const n = 3
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	recs := make([]*httptest.ResponseRecorder, n)
	dones := make([]chan struct{}, n)
	for i := 0; i < n; i++ {
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		recs[i], dones[i] = rec, done
		req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=task.completed", nil).WithContext(ctx)
		go func() {
			restgw.HandleW17Events(rec, req, source, 1*time.Hour)
			close(done)
		}()
	}

	// Let all clients register with the hub.
	time.Sleep(40 * time.Millisecond)

	if got := cf.count.Load(); got != 1 {
		t.Fatalf("expected exactly 1 shared bus subscriber for %d clients; got %d", n, got)
	}

	// One publish → every connected client gets the frame.
	if err := bus.Dispatch(context.Background(), "default", "task.completed", wrapperspb.String(`{"id":"t-1"}`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	for i := 0; i < n; i++ {
		<-dones[i]
	}
	for i, rec := range recs {
		if !strings.Contains(rec.Body.String(), "event: task.completed\n") {
			t.Errorf("client %d missing the fanned-out frame:\n%s", i, rec.Body.String())
		}
	}
}

// TestEventbusEventSource_TranscodeSkip — transcode
// returning (skip=true) drops the delivery without erroring.
// Used for runtime filtering beyond what the topic-name
// filter does (e.g. per-tenant skipping).
func TestEventbusEventSource_TranscodeSkip(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	skipping := func(topic string, envelope []byte) ([]byte, map[string]string, bool, error) {
		return envelope, nil, true, nil // always skip
	}
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, skipping, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=x.y", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(rec, req, source, 1*time.Hour)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	_ = bus.Dispatch(context.Background(), "default", "x.y", wrapperspb.String("any"))
	<-done

	if strings.Contains(rec.Body.String(), "event: x.y") {
		t.Errorf("skip=true delivery shouldn't reach the wire; got:\n%s", rec.Body.String())
	}
}

// TestEventbusEventSource_TranscodeError — transcode
// returning err drops the delivery (treated like skip), the
// SSE connection stays open + the source keeps receiving
// subsequent deliveries.
func TestEventbusEventSource_TranscodeError(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	// calls is bumped from the bus dispatch goroutines (one per
	// delivery) — atomic so the shared-hub handler, which now
	// transcodes on whichever goroutine the bus fires, stays
	// race-clean. (The two deliveries are still serialised by the
	// 20ms sleep below, so first→err / second→ok holds.)
	var calls atomic.Int32
	transcode := func(topic string, envelope []byte) ([]byte, map[string]string, bool, error) {
		if calls.Add(1) == 1 {
			return nil, nil, false, fmt.Errorf("bogus envelope")
		}
		return envelope, nil, false, nil
	}
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, transcode, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=x.y", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(rec, req, source, 1*time.Hour)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	_ = bus.Dispatch(context.Background(), "default", "x.y", wrapperspb.String("first-fails"))
	time.Sleep(20 * time.Millisecond)
	_ = bus.Dispatch(context.Background(), "default", "x.y", wrapperspb.String("second-ok"))
	<-done

	body := rec.Body.String()
	if strings.Count(body, "event: x.y") != 1 {
		t.Errorf("expected exactly one delivered frame (first errored, second OK); got body:\n%s", body)
	}
}

// TestEventbusEventSource_LabelFiltering — a labelled event
// reaches only principals whose labels satisfy it. Drives the
// full consumer path: a fake AuthFunc supplies the principal
// bytes, RequireAuth stashes them, HandleW17Events recovers them,
// and the source derives + matches labels. An event scoped to
// {project_id: alpha} reaches the alpha principal but not beta.
func TestEventbusEventSource_LabelFiltering(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	// Every event is tagged {project_id: "alpha"}.
	transcode := func(topic string, envelope []byte) ([]byte, map[string]string, bool, error) {
		return envelope, map[string]string{"project_id": "alpha"}, false, nil
	}
	// Principal labels = {project_id: <userData string>}.
	principal := func(userData []byte) map[string]string {
		if len(userData) == 0 {
			return nil
		}
		return map[string]string{"project_id": string(userData)}
	}
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, transcode, principal)

	run := func(principalID string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
			return []byte(principalID), nil
		})
		handler := restgw.RequireAuth(authFn, func(w http.ResponseWriter, r *http.Request) {
			restgw.HandleW17Events(w, r, source, 1*time.Hour)
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=p.evt", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { handler(rec, req); close(done) }()
		time.Sleep(30 * time.Millisecond)
		_ = bus.Dispatch(context.Background(), "default", "p.evt", wrapperspb.String("x"))
		<-done
		return rec.Body.String()
	}

	if body := run("alpha"); !strings.Contains(body, "event: p.evt\n") {
		t.Errorf("entitled principal (project_id=alpha) should receive the event; body:\n%s", body)
	}
	if body := run("beta"); strings.Contains(body, "event: p.evt\n") {
		t.Errorf("unentitled principal (project_id=beta) must NOT receive the alpha-scoped event; body:\n%s", body)
	}
}

// TestEventbusEventSource_UnlabelledEventNoCrossTenant is the
// mcpbus-sec-1 regression guard. With tenant isolation active (a
// principal-labels deriver configured), an UNLABELLED event must NOT
// reach a label-bearing (tenant) principal — it would otherwise leak
// across tenants (e.g. the unlabelled w17.invalidate carrying entity
// IDs). It still reaches a no-label principal.
func TestEventbusEventSource_UnlabelledEventNoCrossTenant(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	// Events carry NO labels.
	transcode := func(topic string, envelope []byte) ([]byte, map[string]string, bool, error) {
		return envelope, nil, false, nil
	}
	principal := func(userData []byte) map[string]string {
		if len(userData) == 0 {
			return nil // no-label / anonymous principal
		}
		return map[string]string{"project_id": string(userData)}
	}
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, transcode, principal)

	run := func(principalID string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
			return []byte(principalID), nil
		})
		handler := restgw.RequireAuth(authFn, func(w http.ResponseWriter, r *http.Request) {
			restgw.HandleW17Events(w, r, source, 1*time.Hour)
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=p.evt", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { handler(rec, req); close(done) }()
		time.Sleep(30 * time.Millisecond)
		_ = bus.Dispatch(context.Background(), "default", "p.evt", wrapperspb.String("x"))
		<-done
		return rec.Body.String()
	}

	// Tenant principal must NOT receive the unlabelled event.
	if body := run("alpha"); strings.Contains(body, "event: p.evt\n") {
		t.Errorf("tenant principal received an unlabelled event (cross-tenant leak); body:\n%s", body)
	}
	// Neither does a LABEL-LESS principal — see
	// TestEventbusEventSource_UnlabelledEventDeniedToUnresolvedPrincipal
	// for why that half is not "the broadcast bucket".
	if body := run(""); strings.Contains(body, "event: p.evt\n") {
		t.Errorf("label-less principal received an unlabelled event under isolation; body:\n%s", body)
	}
}

// TestEventbusEventSource_UnlabelledEventDeniedToUnresolvedPrincipal is the
// C8-F11 regression guard, and it is the OTHER half of mcpbus-sec-1.
//
// A label-less principal is not "the principal with no tenant"; under an
// isolating surface it is most often the principal whose tenant could not be
// RESOLVED. The scope decorators deliberately stamp nothing when a caller's
// membership is ambiguous or absent (0 or ≥2 orgs), so a scoped model refuses
// — fail-closed. Routing every unlabelled event to exactly that principal
// inverted the decision: failing to resolve made the least-privileged caller
// the WIDEST audience in the deployment (`len(orgs) != 1` includes zero).
//
// So under active isolation an unlabelled event reaches nobody. The isolating
// deployment's own events must carry the label they are partitioned by; the
// reserved `w17.invalidate` topic does this via InvalidateEvent.labels.
func TestEventbusEventSource_UnlabelledEventDeniedToUnresolvedPrincipal(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	transcode := func(topic string, envelope []byte) ([]byte, map[string]string, bool, error) {
		return envelope, nil, false, nil // unlabelled event
	}
	// An isolating surface: the deriver is configured, and this principal's
	// tenant did not resolve, so it carries NO labels (the fail-closed
	// branch stamps neither scope nor label).
	principal := func(userData []byte) map[string]string { return nil }
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, transcode, principal)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		return []byte("unresolved-principal"), nil
	})
	handler := restgw.RequireAuth(authFn, func(w http.ResponseWriter, r *http.Request) {
		restgw.HandleW17Events(w, r, source, 1*time.Hour)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=p.evt", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler(rec, req); close(done) }()
	time.Sleep(30 * time.Millisecond)
	_ = bus.Dispatch(context.Background(), "default", "p.evt", wrapperspb.String("x"))
	<-done

	if body := rec.Body.String(); strings.Contains(body, "event: p.evt\n") {
		t.Errorf("a principal whose scope did not resolve received an unlabelled event —\n"+
			"failing to resolve must not widen delivery; body:\n%s", body)
	}
}

// TestEventbusEventSource_NoIsolationStillBroadcasts pins the other side of
// the same rule: with NO principal-labels deriver (a surface that declares no
// labels field at all — the single-tenant default), isolation is OFF and an
// unlabelled event still reaches every connected client. The deny above is
// scoped to surfaces that actually partition by label; it must not silence
// the projects that never did.
func TestEventbusEventSource_NoIsolationStillBroadcasts(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	transcode := func(topic string, envelope []byte) ([]byte, map[string]string, bool, error) {
		return envelope, nil, false, nil
	}
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, transcode, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=p.evt", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { restgw.HandleW17Events(rec, req, source, 1*time.Hour); close(done) }()
	time.Sleep(30 * time.Millisecond)
	_ = bus.Dispatch(context.Background(), "default", "p.evt", wrapperspb.String("x"))
	<-done

	if body := rec.Body.String(); !strings.Contains(body, "event: p.evt\n") {
		t.Errorf("with isolation OFF an unlabelled event must still broadcast; body:\n%s", body)
	}
}

// TestEventbusEventSource_NilFactoryRefused — defensive: nil
// factory short-circuits subscribe with a clear error.
func TestEventbusEventSource_NilFactoryRefused(t *testing.T) {
	source := restgw.NewEventbusEventSource(nil, []string{"default"}, rawTranscode, nil)
	_, err := source.Subscribe(context.Background(), []string{"x"}, nil)
	if err == nil {
		t.Fatal("expected error for nil factory")
	}
	if !strings.Contains(err.Error(), "nil factory") {
		t.Errorf("error should mention nil factory; got: %v", err)
	}
}

// TestEventbusEventSource_NilTranscodeRefused — same for
// nil transcode.
func TestEventbusEventSource_NilTranscodeRefused(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, nil, nil)
	_, err := source.Subscribe(context.Background(), []string{"x"}, nil)
	if err == nil {
		t.Fatal("expected error for nil transcode")
	}
	if !strings.Contains(err.Error(), "nil transcode") {
		t.Errorf("error should mention nil transcode; got: %v", err)
	}
}

// --- ensureOpen must not drain while holding the hub lock ---

// drainRunsHandler stands in for a subscriber whose Drain is waiting on an
// in-flight delivery: real Drain blocks on the subscriber's WaitGroup until
// every handler callback has returned (nats.go's R-bus-2 discipline), so
// calling the handler from Drain is a faithful stand-in for that wait.
type drainRunsHandler struct {
	handler   eventbus.HandlerFunc
	drainDone chan struct{}
}

func (s *drainRunsHandler) Subscribe(_ context.Context, _ string, h eventbus.HandlerFunc) error {
	s.handler = h
	return nil
}

func (s *drainRunsHandler) Drain(_ context.Context) error {
	if s.handler != nil {
		// The delivery Drain is waiting for. It reaches the hub, which takes
		// the hub lock — so if the caller drained while holding that lock,
		// this never returns.
		_ = s.handler(context.Background(), "task.created", []byte(`{}`))
	}
	close(s.drainDone)
	return nil
}

// failAfterFirst opens one working subscriber and then refuses, which is the
// partial-failure path ensureOpen has to clean up after.
type failAfterFirst struct {
	first *drainRunsHandler
	n     int
}

func (f *failAfterFirst) Subscriber(_ context.Context, _ string) (eventbus.Subscriber, error) {
	f.n++
	if f.n == 1 {
		return f.first, nil
	}
	return nil, fmt.Errorf("broker unavailable")
}

// A partially-failed open drains what it already subscribed — and must do it
// with the hub lock RELEASED.
//
// Drain waits for in-flight handler callbacks, and a callback lands in
// handleDelivery, which takes the hub lock to check whether any client wants
// the topic. Draining while holding that lock is therefore a lock-order
// cycle: the drain waits for a handler that is waiting for the lock the
// drain's caller holds. Nothing times out — the source opens its subscribers
// against context.Background() — so the whole SSE surface wedges for the
// process lifetime, for every client and every future delivery.
//
// The leak this cleanup exists to fix was found by an audit; the deadlock it
// introduced was found by the next pass, because the fix shipped with no test
// for its own failure path. This is that test.
func TestEventbusEventSource_PartialOpenDrainsWithoutHoldingTheHubLock(t *testing.T) {
	sub := &drainRunsHandler{drainDone: make(chan struct{})}
	source := restgw.NewEventbusEventSource(
		&failAfterFirst{first: sub}, []string{"tasks", "audit"}, rawTranscode, nil)

	returned := make(chan error, 1)
	go func() {
		_, err := source.Subscribe(context.Background(), []string{"task.created"}, nil)
		returned <- err
	}()

	select {
	case <-sub.drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain never returned: the partial-open cleanup is holding the hub lock " +
			"while draining, so the delivery Drain waits for can never take that lock — " +
			"the SSE surface is wedged for the process lifetime")
	}

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("a failed open reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe never returned after the drain completed")
	}
}
