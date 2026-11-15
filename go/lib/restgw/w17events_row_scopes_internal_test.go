package restgw

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// lastDispatch records the envelope the emitter put on the bus.
type lastDispatch struct{ env []byte }

func (b *lastDispatch) Dispatch(_ context.Context, _, _ string, m proto.Message) error {
	var err error
	b.env, err = proto.Marshal(m)
	return err
}

// TestHub_RowScopes: an audience hub delivers a cache invalidation to the
// clients who may READ the rows it names — whoever wrote them. It used to
// compare the scopes of the request that EMITTED it with the client's, so
// a member never heard about her own row changing unless she changed it
// herself: not when staff edited it, not when a job did.
func TestHub_RowScopes(t *testing.T) {
	labelsOf := func(b []byte) map[string]string {
		if len(b) == 0 {
			return nil
		}
		return map[string]string{"org": "x"}
	}
	scopesOf := func(b []byte) map[string]string {
		if len(b) == 0 {
			return nil
		}
		return map[string]string{"org_id": "x", "user_id": string(b)}
	}
	transcode := func(topic string, env []byte) ([]byte, map[string]string, bool, error) {
		if topic != eventbus.W17InvalidateTopic {
			return []byte("{}"), map[string]string{"org": "x"}, false, nil
		}
		var ev w17pb.InvalidateEvent
		if err := proto.Unmarshal(env, &ev); err != nil {
			return nil, nil, false, err
		}
		return []byte("{}"), ev.GetLabels(), false, nil
	}
	newHub := func(owner []string) (*EventbusEventSource, map[string]*hubClient) {
		e := NewEventbusEventSource(eventbus.NewMemoryBus(), []string{"c"}, transcode, labelsOf).
			WithRowScopes([]string{"org_id"}, owner, scopesOf)
		clients := map[string]*hubClient{}
		e.clients = map[*hubClient]struct{}{}
		for _, who := range []string{"alice", "bob", ""} {
			c := &hubClient{
				wanted: map[string]struct{}{"note.changed": {}, eventbus.W17InvalidateTopic: {}},
				labels: labelsOf([]byte(who)), scopes: scopesOf([]byte(who)), out: make(chan Event, 8),
			}
			clients[who] = c
			e.clients[c] = struct{}{}
		}
		return e, clients
	}
	// The scopes of the Note model: org_id (org axis), user_id (owner axis
	// on the owner_id column).
	noteScopes := func(rowOrg, rowOwner string, bypassed bool) []eventbus.RowScope {
		return []eventbus.RowScope{
			{Name: "org_id", Row: rowOrg, Bypassed: bypassed},
			{Name: "user_id", Owner: true, Row: rowOwner, Bypassed: bypassed},
		}
	}
	req := func(kv ...string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs(kv...))
	}
	staff := req("x-w17-scope-org_id", "x", "x-w17-scope-user_id", "staff-1", "x-w17-data-scope", "org", "w17-label-org", "x")
	alice := req("x-w17-scope-org_id", "x", "x-w17-scope-user_id", "alice", "x-w17-data-scope", "owner", "w17-label-org", "x")
	job := context.Background()

	type want struct{ alice, bob, noScopes bool }
	cases := []struct {
		name  string
		owner []string // the hub's owner-axis scopes (nil = organization range)
		ctx   context.Context
		rows  []eventbus.RowScope
		want  want
	}{
		{"staff edits alice's row, the response is the row", []string{"user_id"}, staff, noteScopes("x", "alice", false), want{true, false, false}},
		{"staff edits a row the response does not show (owner unknown)", []string{"user_id"}, staff, noteScopes("", "", false), want{true, true, false}},
		{"alice edits her own row (owner range held it to her)", []string{"user_id"}, alice, noteScopes("", "", false), want{true, false, false}},
		{"a job edits alice's row (no request, scopes bypassed, the row returned)", []string{"user_id"}, job, noteScopes("x", "alice", true), want{true, false, false}},
		{"a job edits a row it does not return (nothing known, no labels)", []string{"user_id"}, job, noteScopes("", "", true), want{false, false, false}},
		{"another organization's row, returned to a job", []string{"user_id"}, job, noteScopes("y", "alice", true), want{false, false, false}},
		{"organization range: staff edits alice's row", nil, staff, noteScopes("x", "alice", false), want{true, true, false}},
		{"organization range: a job edits a row in the organization", nil, job, noteScopes("x", "alice", true), want{true, true, false}},
	}
	for _, c := range cases {
		e, clients := newHub(c.owner)
		bus := &lastDispatch{}
		if err := eventbus.EmitInvalidateRows(c.ctx, bus, "c", "g", "Note", nil, eventbus.RowScopes(c.ctx, c.rows)); err != nil {
			t.Fatal(err)
		}
		if err := e.handleDelivery(context.Background(), "c", eventbus.W17InvalidateTopic, bus.env); err != nil {
			t.Fatal(err)
		}
		got := want{len(clients["alice"].out) > 0, len(clients["bob"].out) > 0, len(clients[""].out) > 0}
		if got != c.want {
			t.Errorf("%s: alice=%v bob=%v no-scopes=%v, want alice=%v bob=%v no-scopes=%v",
				c.name, got.alice, got.bob, got.noScopes, c.want.alice, c.want.bob, c.want.noScopes)
		}
	}

	// Domain events carry a row's DATA. On an owner-range hub one reaches
	// the clients whose reads would return its row, and no one when the
	// row's owner is not known; on an organization-range hub, its
	// organization. An event that carries no row reaches no one on an
	// owner-range hub (labels partition by organization only), and its
	// labels' organization on an organization-range one.
	type row struct {
		scopes map[string]string
		ok     bool
	}
	domainCases := []struct {
		name  string
		owner []string
		row   *row // nil: no row reader configured
		want  want
	}{
		{"staff's NoteChanged on alice's row", []string{"user_id"}, &row{map[string]string{"org_id": "x", "user_id": "alice"}, true}, want{true, false, false}},
		{"a row whose owner the event does not carry", []string{"user_id"}, &row{map[string]string{"org_id": "x", "user_id": ""}, true}, want{false, false, false}},
		{"another organization's row", []string{"user_id"}, &row{map[string]string{"org_id": "y", "user_id": "alice"}, true}, want{false, false, false}},
		{"a row of a model with no owner scope", []string{"user_id"}, &row{map[string]string{"org_id": "x"}, true}, want{true, true, false}},
		{"an event that carries no row, owner range", []string{"user_id"}, &row{nil, false}, want{false, false, false}},
		{"no row reader, owner range", []string{"user_id"}, nil, want{false, false, false}},
		{"organization range: alice's row", nil, &row{map[string]string{"org_id": "x", "user_id": "alice"}, true}, want{true, true, false}},
		{"organization range: an event that carries no row", nil, &row{nil, false}, want{true, true, false}},
	}
	for _, c := range domainCases {
		e, clients := newHub(c.owner)
		if c.row != nil {
			r := c.row
			e.WithEventRows(func(topic string, _ []byte) (map[string]string, bool) {
				if topic != "note.changed" {
					t.Errorf("rows read for topic %q", topic)
				}
				return r.scopes, r.ok
			})
		}
		if err := e.handleDelivery(context.Background(), "c", "note.changed", []byte{}); err != nil {
			t.Fatal(err)
		}
		got := want{len(clients["alice"].out) > 0, len(clients["bob"].out) > 0, len(clients[""].out) > 0}
		if got != c.want {
			t.Errorf("%s: alice=%v bob=%v no-scopes=%v, want alice=%v bob=%v no-scopes=%v",
				c.name, got.alice, got.bob, got.noScopes, c.want.alice, c.want.bob, c.want.noScopes)
		}
	}

	// An entity the audience widens to the organization: its
	// invalidations are not narrowed by owner — bob's widened list shows
	// alice's row, and hears about it changing.
	e, clients := newHub([]string{"user_id"})
	e.WithOwnerLifted("Note")
	bus := &lastDispatch{}
	if err := eventbus.EmitInvalidateRows(staff, bus, "c", "g", "Note", nil, eventbus.RowScopes(staff, noteScopes("x", "alice", false))); err != nil {
		t.Fatal(err)
	}
	if err := e.handleDelivery(context.Background(), "c", eventbus.W17InvalidateTopic, bus.env); err != nil {
		t.Fatal(err)
	}
	if len(clients["alice"].out) != 1 || len(clients["bob"].out) != 1 || len(clients[""].out) != 0 {
		t.Errorf("a widened entity's invalidation: alice=%d bob=%d no-scopes=%d, want 1 1 0", len(clients["alice"].out), len(clients["bob"].out), len(clients[""].out))
	}
	// Another entity on the same hub is still narrowed by owner.
	e, clients = newHub([]string{"user_id"})
	e.WithOwnerLifted("Task")
	if err := e.handleDelivery(context.Background(), "c", eventbus.W17InvalidateTopic, bus.env); err != nil {
		t.Fatal(err)
	}
	if len(clients["alice"].out) != 1 || len(clients["bob"].out) != 0 {
		t.Errorf("an entity not widened: alice=%d bob=%d, want 1 0", len(clients["alice"].out), len(clients["bob"].out))
	}
}
