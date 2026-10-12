package eventbus_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// envelopeDesc is an envelope shaped like the generated ones: a payload
// field, `map<string, string> metadata = 50001` and
// `map<string, string> labels = 50002`. Built at runtime so the test encodes
// with the real protobuf map encoding rather than hand-written bytes.
var envelopeDesc = func() protoreflect.MessageDescriptor {
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	entry := func(name string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name: proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("key"), Number: proto.Int32(1), Type: str, Label: opt, JsonName: proto.String("key")},
				{Name: proto.String("value"), Number: proto.Int32(2), Type: str, Label: opt, JsonName: proto.String("value")},
			},
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
		}
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("w17test/envelope.proto"),
		Package: proto.String("w17test"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Envelope"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("note"), Number: proto.Int32(1), Type: str, Label: opt, JsonName: proto.String("note")},
				{Name: proto.String("metadata"), Number: proto.Int32(50001), Type: msg, Label: rep, TypeName: proto.String(".w17test.Envelope.MetadataEntry"), JsonName: proto.String("metadata")},
				{Name: proto.String("labels"), Number: proto.Int32(50002), Type: msg, Label: rep, TypeName: proto.String(".w17test.Envelope.LabelsEntry"), JsonName: proto.String("labels")},
			},
			NestedType: []*descriptorpb.DescriptorProto{entry("MetadataEntry"), entry("LabelsEntry")},
		}},
	}, nil)
	if err != nil {
		panic(err)
	}
	return fd.Messages().ByName("Envelope")
}()

func envelope(note string, metadata, labels map[string]string) proto.Message {
	m := dynamicpb.NewMessage(envelopeDesc)
	m.Set(envelopeDesc.Fields().ByName("note"), protoreflect.ValueOfString(note))
	for field, kv := range map[string]map[string]string{"metadata": metadata, "labels": labels} {
		mp := m.Mutable(envelopeDesc.Fields().ByName(protoreflect.Name(field))).Map()
		for k, v := range kv {
			mp.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfString(v))
		}
	}
	return m
}

// noteOf decodes the payload field of a delivered envelope.
func noteOf(t *testing.T, raw []byte) string {
	t.Helper()
	m := dynamicpb.NewMessage(envelopeDesc)
	if err := proto.Unmarshal(raw, m); err != nil {
		t.Fatalf("unmarshal delivered envelope: %v", err)
	}
	return m.Get(envelopeDesc.Fields().ByName("note")).String()
}

// The cases every transport is driven through. "refused" envelopes carry a
// transaction key in their metadata; the others are decoys that must still
// be delivered: the key as a value, in labels, as a longer name, and the
// ordinary trace and scope keys an event does carry.
var guardCases = []struct {
	note     string
	metadata map[string]string
	labels   map[string]string
	refused  bool
}{
	{note: "tx-id", metadata: map[string]string{"traceparent": "00-x", "w17-tx-id": "3f1c"}, refused: true},
	{note: "conn-id", metadata: map[string]string{"w17-conn-id": "c1"}, refused: true},
	{note: "tx-id-upper", metadata: map[string]string{"W17-Tx-Id": "3f1c"}, refused: true},
	{note: "both", metadata: map[string]string{"w17-tx-id": "a", "w17-conn-id": "b", "x-w17-user": "u"}, refused: true},
	{note: "plain", metadata: map[string]string{"traceparent": "00-x", "x-w17-scope-tenant_id": "t1"}},
	{note: "key-as-value", metadata: map[string]string{"x-note": "w17-tx-id"}},
	{note: "key-in-labels", labels: map[string]string{"w17-tx-id": "a", "w17-conn-id": "b"}},
	{note: "longer-name", metadata: map[string]string{"w17-tx-id-hint": "a", "x-w17-conn-id": "b"}},
	{note: "no-metadata"},
}

// driveGuard dispatches every case on bus and waits until each decoy has been
// delivered and each refused case has failed at least once. It fails the test
// if a refused envelope ever reaches the handler.
type guardBus interface {
	eventbus.Dispatcher
	eventbus.SubscriberFactory
}

func driveGuard(t *testing.T, bus guardBus, obs *recordingObserver) {
	t.Helper()
	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var mu sync.Mutex
	delivered := map[string]bool{}
	if err := sub.Subscribe(context.Background(), "**", func(_ context.Context, _ string, raw []byte) error {
		mu.Lock()
		defer mu.Unlock()
		delivered[noteOf(t, raw)] = true
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	refused := 0
	for _, c := range guardCases {
		if err := bus.Dispatch(context.Background(), "default", "guard."+c.note, envelope(c.note, c.metadata, c.labels)); err != nil {
			t.Fatalf("Dispatch %s: %v", c.note, err)
		}
		if c.refused {
			refused++
		}
	}
	waitFor(t, 10*time.Second, "every decoy delivered and every refused envelope failed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range guardCases {
			if !c.refused && !delivered[c.note] {
				return false
			}
		}
		return len(failedTopics(obs)) >= refused
	})
	mu.Lock()
	defer mu.Unlock()
	for _, c := range guardCases {
		if c.refused && delivered[c.note] {
			t.Errorf("%s: envelope with a transaction key reached the handler", c.note)
		}
	}
	for topic, msg := range failedTopics(obs) {
		if !strings.Contains(msg, eventbus.ErrTransactionKeyInEnvelope.Error()) {
			t.Errorf("%s failed with %q, want ErrTransactionKeyInEnvelope", topic, msg)
		}
	}
}

// failedTopics maps each topic with a delivery failure to its last error.
func failedTopics(obs *recordingObserver) map[string]string {
	obs.mu.Lock()
	defer obs.mu.Unlock()
	out := map[string]string{}
	for _, r := range obs.deliverFailure {
		out[r.Topic] = r.Err
	}
	return out
}

func TestEnvelopeGuard_Memory(t *testing.T) {
	obs := &recordingObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{Observer: obs})
	driveGuard(t, bus, obs)
}

func TestEnvelopeGuard_NATS_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:               startEmbeddedNATS(t),
		DurablePrefix:     "guard",
		DefaultMaxDeliver: 2,
		DefaultAckWait:    300 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	driveGuard(t, bus, obs)
}

func TestEnvelopeGuard_Redis_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               startEmbeddedRedis(t),
		GroupPrefix:       "guard",
		DefaultMaxDeliver: 2,
		DefaultAckWait:    200 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	driveGuard(t, bus, obs)
}

// A refused envelope ends in the dead-letter queue like any delivery that
// keeps failing; the guard adds no path of its own.
func TestEnvelopeGuard_RefusedEnvelopeIsDeadLettered_Redis(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               dsn,
		GroupPrefix:       "guard-dlq",
		DefaultMaxDeliver: 2,
		DefaultAckWait:    200 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	sub, _ := bus.Subscriber(context.Background(), "default")
	called := make(chan struct{}, 1)
	_ = sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		called <- struct{}{}
		return nil
	})
	_ = bus.Dispatch(context.Background(), "default", "t", envelope("x", map[string]string{"w17-tx-id": "a"}, nil))
	waitFor(t, 8*time.Second, "dead-lettered", func() bool {
		_, _, _, _, drop := obs.snapshots()
		return drop >= 1
	})
	if n, err := rawRedisClient(t, dsn).XLen(context.Background(), "default.dlq").Result(); err != nil || n < 1 {
		t.Fatalf("default.dlq has %d entries (%v), want >= 1", n, err)
	}
	select {
	case <-called:
		t.Fatal("handler ran for a refused envelope")
	default:
	}
}

// An SDK publisher never produces a refused envelope: the emit side drops
// every key the delivery side refuses.
func TestEnvelopeGuard_RefusedKeysAreStrippedOnEmit(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"w17-tx-id", "a", "w17-conn-id", "b", "traceparent", "00-x"))
	md := eventbus.MetadataFromContext(ctx)
	for _, k := range []string{"w17-tx-id", "w17-conn-id"} {
		if _, ok := md[k]; ok {
			t.Errorf("%s is refused on delivery but captured on emit", k)
		}
	}
	if md["traceparent"] != "00-x" {
		t.Errorf("decoy traceparent not captured: %v", md)
	}
}
