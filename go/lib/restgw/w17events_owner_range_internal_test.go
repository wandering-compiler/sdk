package restgw

import (
	"context"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// envelopeWithMetadata hand-encodes a domain event envelope carrying only
// its metadata map (field 50001, the number every generated envelope uses).
func envelopeWithMetadata(md map[string]string) []byte {
	var b []byte
	for k, v := range md {
		var entry []byte
		entry = protowire.AppendTag(entry, 1, protowire.BytesType)
		entry = protowire.AppendString(entry, k)
		entry = protowire.AppendTag(entry, 2, protowire.BytesType)
		entry = protowire.AppendString(entry, v)
		b = protowire.AppendTag(b, eventbus.EnvelopeMetadataField, protowire.BytesType)
		b = protowire.AppendBytes(b, entry)
	}
	return b
}

// TestHub_OwnerRange: on an audience gateway whose range is the caller's
// own rows, the hub delivered by organization label only, so a staff
// member's write in organization X reached every client of X — a row the
// client's own reads would never return. With the owner range the event
// reaches a client only when the request that emitted it carried that
// client's owner scope; one that carries none reaches nobody.
func TestHub_OwnerRange(t *testing.T) {
	labelsOf := func(b []byte) map[string]string { return map[string]string{"org": "x"} }
	scopesOf := func(b []byte) map[string]string {
		return map[string]string{"org_id": "x", "owner_id": string(b)}
	}
	transcode := func(topic string, env []byte) ([]byte, map[string]string, bool, error) {
		return []byte("{}"), map[string]string{"org": "x"}, false, nil
	}
	e := NewEventbusEventSource(eventbus.NewMemoryBus(), []string{"c"}, transcode, labelsOf).
		WithOwnerRange([]string{"owner_id"}, scopesOf)
	alice := &hubClient{wanted: map[string]struct{}{"note.changed": {}, eventbus.W17InvalidateTopic: {}}, labels: labelsOf(nil), out: make(chan Event, 8)}
	bob := &hubClient{wanted: alice.wanted, labels: labelsOf(nil), out: make(chan Event, 8)}
	alice.scopes, bob.scopes = scopesOf([]byte("alice")), scopesOf([]byte("bob"))
	e.clients = map[*hubClient]struct{}{alice: {}, bob: {}}

	deliver := func(topic string, env []byte) (gotAlice, gotBob bool) {
		if err := e.handleDelivery(context.Background(), "c", topic, env); err != nil {
			t.Fatal(err)
		}
		select {
		case <-alice.out:
			gotAlice = true
		default:
		}
		select {
		case <-bob.out:
			gotBob = true
		default:
		}
		return
	}
	inv, _ := proto.Marshal(&w17pb.InvalidateEvent{Group: "g", Message: "Note", Ids: []string{"7"},
		Labels: map[string]string{"org": "x"}, Scopes: map[string]string{"x-w17-scope-owner_id": "alice", "x-w17-scope-org_id": "x"}})
	cases := []struct {
		name      string
		topic     string
		env       []byte
		wantAlice bool
		wantBob   bool
	}{
		{"alice's own write", "note.changed", envelopeWithMetadata(map[string]string{"x-w17-scope-owner_id": "alice", "x-w17-scope-org_id": "x"}), true, false},
		{"a staff member's write in the organization", "note.changed", envelopeWithMetadata(map[string]string{"x-w17-scope-owner_id": "staff-1", "x-w17-scope-org_id": "x"}), false, false},
		{"an event no request scoped", "note.changed", envelopeWithMetadata(nil), false, false},
		{"alice's own cache invalidation", eventbus.W17InvalidateTopic, inv, true, false},
	}
	for _, c := range cases {
		a, b := deliver(c.topic, c.env)
		if a != c.wantAlice || b != c.wantBob {
			t.Errorf("%s: alice=%v bob=%v, want alice=%v bob=%v", c.name, a, b, c.wantAlice, c.wantBob)
		}
	}
}
