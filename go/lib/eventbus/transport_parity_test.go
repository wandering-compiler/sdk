package eventbus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// parityBus is one transport under a name, for the tests that must hold on
// every transport alike.
type parityBus struct {
	name string
	bus  interface {
		eventbus.Dispatcher
		eventbus.SubscriberFactory
	}
	obs *recordingObserver
}

func everyTransport(t *testing.T) []parityBus {
	t.Helper()
	memObs, natsObs, redisObs := &recordingObserver{}, &recordingObserver{}, &recordingObserver{}
	mem := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{Observer: memObs})
	nb, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN: startEmbeddedNATS(t), DurablePrefix: "parity", Observer: natsObs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = nb.Close(timeout(t, 5*time.Second)) })
	rb, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN: startEmbeddedRedis(t), GroupPrefix: "parity", Observer: redisObs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = rb.Close(timeout(t, 5*time.Second)) })
	return []parityBus{{"memory", mem, memObs}, {"nats", nb, natsObs}, {"redis", rb, redisObs}}
}

// An empty topic is refused identically on every transport, and the refusal
// is reported like any failed emit. (NATS used to publish it outside the
// stream's capture and fail; memory and Redis accepted it.)
func TestDispatch_EmptyTopicRefusedOnEveryTransport(t *testing.T) {
	for _, tr := range everyTransport(t) {
		err := tr.bus.Dispatch(context.Background(), "default", "", wrapperspb.String("x"))
		if !errors.Is(err, eventbus.ErrInvalidTopic) {
			t.Errorf("%s: Dispatch(empty topic) = %v, want ErrInvalidTopic", tr.name, err)
		}
		if _, fail, _, _, _ := tr.obs.snapshots(); fail != 1 {
			t.Errorf("%s: OnEmitFailure fired %d times, want 1", tr.name, fail)
		}
	}
}

// A channel name one broker cannot represent is refused on every transport,
// before it reaches a broker, at both subscribe and emit.
func TestChannelNameRefusedOnEveryTransport(t *testing.T) {
	for _, tr := range everyTransport(t) {
		for _, ch := range []string{"a.b", "has space", "", "wild*", "x>"} {
			if _, err := tr.bus.Subscriber(context.Background(), ch); !errors.Is(err, eventbus.ErrInvalidChannel) {
				t.Errorf("%s: Subscriber(%q) err = %v, want ErrInvalidChannel", tr.name, ch, err)
			}
			if err := tr.bus.Dispatch(context.Background(), ch, "t", wrapperspb.String("x")); !errors.Is(err, eventbus.ErrInvalidChannel) {
				t.Errorf("%s: Dispatch(%q) err = %v, want ErrInvalidChannel", tr.name, ch, err)
			}
		}
		for _, topic := range []string{"a..b", ".a", "a.", "a b", "a.*", "a.>"} {
			if err := tr.bus.Dispatch(context.Background(), "default", topic, wrapperspb.String("x")); !errors.Is(err, eventbus.ErrInvalidTopic) {
				t.Errorf("%s: Dispatch(topic %q) err = %v, want ErrInvalidTopic", tr.name, topic, err)
			}
		}
		// The names every generated surface uses stay valid.
		if _, err := tr.bus.Subscriber(context.Background(), "default"); err != nil {
			t.Errorf("%s: Subscriber(default): %v", tr.name, err)
		}
		if err := tr.bus.Dispatch(context.Background(), "default", "admin_auth.auth.role.created", wrapperspb.String("x")); err != nil {
			t.Errorf("%s: Dispatch(valid): %v", tr.name, err)
		}
	}
}

// Per-channel option maps and the NATS consumer-name prefix are validated at
// construction, so a name that can never work fails at boot.
func TestBusConstruction_RefusesInvalidNames(t *testing.T) {
	if _, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN: "nats://127.0.0.1:1", DurablePrefix: "p",
		ChannelRetries: map[string]eventbus.ChannelRetry{"a.b": {}},
	}); !errors.Is(err, eventbus.ErrInvalidChannel) {
		t.Errorf("NewNatsBus(ChannelRetries[a.b]) err = %v, want ErrInvalidChannel", err)
	}
	if _, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN: "nats://127.0.0.1:1", DurablePrefix: "my.app",
	}); err == nil || errors.Is(err, natsgo.ErrNoServers) {
		t.Errorf("NewNatsBus(DurablePrefix my.app) err = %v, want a refusal before connecting", err)
	}
	if _, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN: "redis://127.0.0.1:1", GroupPrefix: "p",
		ChannelRetries: map[string]eventbus.ChannelRetry{"a.b": {}},
	}); !errors.Is(err, eventbus.ErrInvalidChannel) {
		t.Errorf("NewRedisBus(ChannelRetries[a.b]) err = %v, want ErrInvalidChannel", err)
	}
}

type ctxKey struct{}

// A MemoryBus handler runs with its subscription's context, like a broker
// delivery — never with the emitting request's values or metadata.
func TestMemoryBus_HandlerContextIsNotTheEmitters(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	subCtx := context.WithValue(context.Background(), ctxKey{}, "subscriber")
	got := make(chan context.Context, 1)
	if err := sub.Subscribe(subCtx, "**", func(ctx context.Context, _ string, _ []byte) error {
		got <- ctx
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	emitCtx := context.WithValue(context.Background(), ctxKey{}, "emitter")
	emitCtx = metadata.NewIncomingContext(emitCtx, metadata.Pairs("w17-tx-id", "tx-1", "x-w17-scope-org_id", "org-1"))
	emitCtx = metadata.AppendToOutgoingContext(emitCtx, "w17-tx-id", "tx-1")
	if err := bus.Dispatch(emitCtx, "default", "t", wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	select {
	case ctx = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}
	if v := ctx.Value(ctxKey{}); v != "subscriber" {
		t.Errorf("handler ctx value = %v, want the subscription's (not the emitter's)", v)
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md) > 0 {
		t.Errorf("handler sees the emitter's incoming metadata: %v", md)
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok && len(md) > 0 {
		t.Errorf("handler sees the emitter's outgoing metadata: %v", md)
	}
}

func natsStreamInfo(t *testing.T, dsn, name string) *jetstream.StreamInfo {
	t.Helper()
	nc, err := natsgo.Connect(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	st, err := js.Stream(timeout(t, 5*time.Second), name)
	if err != nil {
		t.Fatalf("stream %q: %v", name, err)
	}
	info, err := st.Info(timeout(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// Streams the NATS adapter creates carry retention limits: the defaults when
// nothing is configured, the configured ones otherwise.
func TestNatsBus_StreamRetention_Embedded(t *testing.T) {
	dsn := startEmbeddedNATS(t)
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: dsn, DurablePrefix: "ret", Observer: eventbus.NopObserver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	sub, err := bus.Subscriber(timeout(t, 5*time.Second), "defaults")
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Subscribe(timeout(t, 5*time.Second), "**", func(context.Context, string, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := natsStreamInfo(t, dsn, "defaults").Config.MaxAge; got != eventbus.DefaultStreamMaxAge {
		t.Errorf("default stream MaxAge = %v, want %v", got, eventbus.DefaultStreamMaxAge)
	}
	if got := natsStreamInfo(t, dsn, "defaults_dlq").Config.MaxAge; got != eventbus.DefaultDLQMaxAge {
		t.Errorf("default DLQ MaxAge = %v, want %v", got, eventbus.DefaultDLQMaxAge)
	}

	capped, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN: dsn, DurablePrefix: "ret2", Observer: eventbus.NopObserver{},
		Retention: eventbus.StreamRetention{MaxAge: -1, MaxMsgs: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = capped.Close(timeout(t, 5*time.Second)) })
	for i := 0; i < 3; i++ {
		if err := capped.Dispatch(context.Background(), "capped", "a.b", wrapperspb.String("x")); err != nil {
			t.Fatal(err)
		}
	}
	info := natsStreamInfo(t, dsn, "capped")
	if info.Config.MaxAge != 0 || info.State.Msgs != 2 {
		t.Errorf("capped stream: MaxAge=%v msgs=%d, want no age limit and 2 messages", info.Config.MaxAge, info.State.Msgs)
	}
}

// The Redis adapter trims its streams on append: by count and by age.
func TestRedisBus_StreamRetention_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	raw := rawRedisClient(t, dsn)
	byCount, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN: dsn, GroupPrefix: "ret", Observer: eventbus.NopObserver{},
		Retention: eventbus.StreamRetention{MaxAge: -1, MaxMsgs: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = byCount.Close(timeout(t, 5*time.Second)) })
	for i := 0; i < 3; i++ {
		if err := byCount.Dispatch(context.Background(), "counted", "a.b", wrapperspb.String("x")); err != nil {
			t.Fatal(err)
		}
	}
	if n := raw.XLen(context.Background(), "counted").Val(); n != 2 {
		t.Errorf("XLEN counted = %d, want 2", n)
	}

	byAge, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN: dsn, GroupPrefix: "ret", Observer: eventbus.NopObserver{},
		Retention: eventbus.StreamRetention{MaxAge: 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = byAge.Close(timeout(t, 5*time.Second)) })
	if err := byAge.Dispatch(context.Background(), "aged", "a.b", wrapperspb.String("old")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := byAge.Dispatch(context.Background(), "aged", "a.b", wrapperspb.String("new")); err != nil {
		t.Fatal(err)
	}
	if n := raw.XLen(context.Background(), "aged").Val(); n != 1 {
		t.Errorf("XLEN aged = %d, want 1 (the older entry trimmed by age)", n)
	}
}

// During a broker outage a NATS emit fails within PublishTimeout instead of
// holding the committing goroutine for the client's request timeout.
func TestNatsBus_PublishTimeoutBoundsOutage_Embedded(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server not ready")
	}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN: srv.ClientURL(), DurablePrefix: "outage", Observer: eventbus.NopObserver{},
		PublishTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	if err := bus.Dispatch(context.Background(), "outage", "a.b", wrapperspb.String("x")); err != nil {
		t.Fatalf("Dispatch while up: %v", err)
	}
	srv.Shutdown()

	start := time.Now()
	err = bus.Dispatch(context.Background(), "outage", "a.b", wrapperspb.String("x"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Dispatch during outage succeeded")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Dispatch during outage took %v, want it bounded by PublishTimeout (300ms)", elapsed)
	}
}
