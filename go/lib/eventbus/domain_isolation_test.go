package eventbus_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

type domainBus interface {
	eventbus.Dispatcher
	eventbus.SubscriberFactory
}

// assertDomainsDoNotShareEnvelopes is the regression guard for two domains
// that declare the same channel name on one broker.
//
// Channel names are unique per domain, not per broker, and one project runs
// one broker: both domains' buses ensure the same stream, `events`, and each
// domain's `**` subscriber read the other domain's envelopes — with that
// domain's principal and scopes in their metadata — and, unable to decode
// them, dead-lettered them into the shared DLQ. A bus with a Domain stamps it
// on every emit, and its subscribers settle a message stamped with another
// domain without handing it to the handler. A message with no stamp (from a
// producer without a Domain, as every producer was before) is delivered as
// before.
func assertDomainsDoNotShareEnvelopes(t *testing.T, newBus func(domain, prefix string) domainBus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a := newBus("domain-a", "domain-a-subscribers")
	b := newBus("domain-b", "domain-b-subscribers")
	legacy := newBus("", "legacy-subscribers")

	recB, recLegacy := &topicRecorder{}, &topicRecorder{}
	for bus, rec := range map[domainBus]*topicRecorder{b: recB, legacy: recLegacy} {
		sub, err := bus.Subscriber(ctx, "events")
		if err != nil {
			t.Fatalf("Subscriber: %v", err)
		}
		if err := sub.Subscribe(ctx, "**", rec.handle); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
	}
	if err := a.Dispatch(ctx, "events", "a.user.created", wrapperspb.String("a")); err != nil {
		t.Fatalf("Dispatch a: %v", err)
	}
	if err := b.Dispatch(ctx, "events", "b.user.created", wrapperspb.String("b")); err != nil {
		t.Fatalf("Dispatch b: %v", err)
	}
	if err := legacy.Dispatch(ctx, "events", "legacy.user.created", wrapperspb.String("l")); err != nil {
		t.Fatalf("Dispatch legacy: %v", err)
	}
	waitFor(t, 10*time.Second, "both subscribers received their events", func() bool {
		return len(recB.got()) >= 2 && len(recLegacy.got()) >= 3
	})
	time.Sleep(300 * time.Millisecond)
	if got := strings.Join(recB.got(), ","); got != "b.user.created,legacy.user.created" {
		t.Errorf("domain-b's subscriber received [%s], want its own event and the unstamped one, not domain-a's", got)
	}
	if got := strings.Join(recLegacy.got(), ","); got != "a.user.created,b.user.created,legacy.user.created" {
		t.Errorf("a subscriber without a Domain received [%s], want every event as before", got)
	}
}

func TestDomainIsolation_Nats_Embedded(t *testing.T) {
	url := startEmbeddedNATS(t)
	assertDomainsDoNotShareEnvelopes(t, func(domain, prefix string) domainBus {
		bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: url, DurablePrefix: prefix, Domain: domain})
		if err != nil {
			t.Fatalf("NewNatsBus: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close(context.Background()) })
		return bus
	})
}

func TestDomainIsolation_Redis_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	assertDomainsDoNotShareEnvelopes(t, func(domain, prefix string) domainBus {
		bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: dsn, GroupPrefix: prefix, Domain: domain})
		if err != nil {
			t.Fatalf("NewRedisBus: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close(context.Background()) })
		return bus
	})
}

func TestDomainOption_RefusesAnInvalidName(t *testing.T) {
	if _, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: "nats://127.0.0.1:1", DurablePrefix: "p", Domain: "a b", ConnectWait: -1}); err == nil || !strings.Contains(err.Error(), "Domain") {
		t.Errorf("NewNatsBus with Domain %q: err = %v, want a Domain refusal", "a b", err)
	}
	if _, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: "redis://127.0.0.1:1", GroupPrefix: "p", Domain: "a\nb"}); err == nil || !strings.Contains(err.Error(), "Domain") {
		t.Errorf("NewRedisBus with Domain %q: err = %v, want a Domain refusal", "a\\nb", err)
	}
}
