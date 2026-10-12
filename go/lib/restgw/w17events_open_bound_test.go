package restgw_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// stuckDrainSub fails its Subscribe and then never finishes draining on
// its own: only the caller's context ends the Drain.
type stuckDrainSub struct{}

func (stuckDrainSub) Subscribe(context.Context, string, eventbus.HandlerFunc) error {
	return errors.New("injected: subscribe failed")
}

func (stuckDrainSub) Drain(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type stuckDrainFactory struct{}

func (stuckDrainFactory) Subscriber(context.Context, string) (eventbus.Subscriber, error) {
	return stuckDrainSub{}, nil
}

// The cleanup drain of a failed open is bounded at 10s: a subscriber that
// never finishes draining does not hold the client's Subscribe past it.
// Takes 10s by construction.
func TestEventbusEventSource_FailedOpenDrainIsBoundedAtTenSeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("10s drain bound")
	}
	src := restgw.NewEventbusEventSource(stuckDrainFactory{}, []string{"tasks"}, hubTranscode, nil)
	start := time.Now()
	if _, err := subscribeWithin(t, src, 20*time.Second); err == nil {
		t.Fatal("expected the injected subscribe error")
	}
	if took := time.Since(start); took < 8*time.Second || took > 13*time.Second {
		t.Errorf("Subscribe returned after %v, want the failed open's drain bounded at ~10s", took.Round(100*time.Millisecond))
	}
}
