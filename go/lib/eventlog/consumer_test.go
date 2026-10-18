package eventlog

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/lib/redact"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

func init() {
	redact.Register(map[string]redact.Plan{
		"grpc.health.v1.HealthCheckRequest":  {},
		"grpc.health.v1.HealthCheckResponse": {},
	})
}

var healthMethod = &Method{FullName: "/grpc.health.v1.Health/Check", RequestTag: 5, ResponseTag: 6}

type funcConsumer struct {
	marketing func(context.Context, *ConsumedBatch) error
	audit     func(context.Context, *ConsumedBatch) error
}

func (f funcConsumer) ConsumeMarketing(ctx context.Context, b *ConsumedBatch) error {
	return f.marketing(ctx, b)
}
func (f funcConsumer) ConsumeAudit(ctx context.Context, b *ConsumedBatch) error {
	return f.audit(ctx, b)
}

// recordTwo publishes a signed-in and an anonymous request on the rest
// surface of domain tasks.
func recordTwo(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	r, err := NewRecorder(context.Background(), RecorderConfig{
		Domain:   "tasks",
		Surfaces: map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Marketing: true, Audit: true}},
		Auth:     AuthFields{Scopes: scopesField, MarketingConsent: consentField},
		Sink: func(api w17pb.RequestApi) func(context.Context, *Batch) error {
			return (&NATSSink{JS: js, Subject: Subject("tasks", api)}).Sink
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureStream(context.Background(), js, StreamName, []string{SubjectPrefix + ".>"}, StreamLimits{}); err != nil {
		t.Fatal(err)
	}
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	signed := s.Start(healthMethod)
	signed.SetRequest(&healthpb.HealthCheckRequest{Service: "tasks"})
	signed.SetResult(&healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil)
	signed.SetCaller(authMD("u-1", true, "org-1"))
	signed.Finish(context.Background(), 200)
	anon := s.Start(healthMethod)
	anon.SetRequest(&healthpb.HealthCheckRequest{Service: "anon"})
	anon.Finish(context.Background(), 401)
	closeRecorder(t, r)
}

func runConsumers(t *testing.T, cs *Consumers, js jetstream.JetStream, cfg ConsumerConfig) (stop func()) {
	t.Helper()
	cfg.JS = js
	if cfg.DurablePrefix == "" {
		cfg.DurablePrefix = "test"
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cs.Run(ctx, cfg) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Each consumer reads its own purpose's records, decoded with the method's
// own types; a marketing consumer that fails is offered the batch again
// without the audit consumer seeing it twice.
func TestConsumers_EachItsPurposeAndItsOwnProgress(t *testing.T) {
	_, js := embeddedJetStream(t)
	recordTwo(t, js)

	var mu sync.Mutex
	var marketing, audit [][]ConsumedRecord
	var mCalls, aCalls atomic.Int32
	cs := &Consumers{}
	c := funcConsumer{
		marketing: func(_ context.Context, b *ConsumedBatch) error {
			if mCalls.Add(1) == 1 {
				return errors.New("the marketing tool is down")
			}
			mu.Lock()
			defer mu.Unlock()
			marketing = append(marketing, b.Records)
			return nil
		},
		audit: func(_ context.Context, b *ConsumedBatch) error {
			aCalls.Add(1)
			mu.Lock()
			defer mu.Unlock()
			audit = append(audit, b.Records)
			return nil
		},
	}
	if err := cs.Marketing("crm", c); err != nil {
		t.Fatal(err)
	}
	if err := cs.Audit("trail", c, "w17.requests.tasks.>"); err != nil {
		t.Fatal(err)
	}
	stop := runConsumers(t, cs, js, ConsumerConfig{Backoff: func(int) time.Duration { return 50 * time.Millisecond }})
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(marketing) == 1 && len(audit) == 1 })
	stop()

	if aCalls.Load() != 1 || mCalls.Load() != 2 {
		t.Fatalf("audit called %d times, marketing %d (want 1 and 2)", aCalls.Load(), mCalls.Load())
	}
	if len(audit[0]) != 2 || len(marketing[0]) != 1 {
		t.Fatalf("audit got %d records, marketing %d (want 2 and 1)", len(audit[0]), len(marketing[0]))
	}
	m := marketing[0][0]
	req, ok := m.Request.(*healthpb.HealthCheckRequest)
	if !ok || req.GetService() != "tasks" || m.Meta.GetUserId() != "u-1" || !m.Meta.GetMarketingConsent() {
		t.Fatalf("marketing record %+v", m)
	}
	resp, ok := m.Response.(*healthpb.HealthCheckResponse)
	if !ok || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("response %v", m.Response)
	}
	if anon := audit[0][1]; anon.Meta.GetUserId() != "" || anon.Meta.GetHttpStatus() != 401 || anon.Response != nil {
		t.Fatalf("anonymous audit record %+v", anon.Meta)
	}
}

// A batch its consumer keeps failing goes to the dead letter subject after
// MaxDeliver offers, with the reason — never lost, never offered forever.
func TestConsumers_DeadLetterAfterMaxDeliver(t *testing.T) {
	_, js := embeddedJetStream(t)
	recordTwo(t, js)
	var calls atomic.Int32
	cs := &Consumers{}
	_ = cs.Audit("broken", funcConsumer{audit: func(context.Context, *ConsumedBatch) error {
		calls.Add(1)
		panic("consumer bug")
	}})
	stop := runConsumers(t, cs, js, ConsumerConfig{MaxDeliver: 3, Backoff: func(int) time.Duration { return 20 * time.Millisecond }})
	var dl jetstream.Stream
	waitUntil(t, func() bool {
		s, err := js.Stream(context.Background(), DeadLetterStream)
		if err != nil {
			return false
		}
		info, err := s.Info(context.Background())
		dl = s
		return err == nil && info.State.Msgs == 1
	})
	stop()
	if calls.Load() != 3 {
		t.Fatalf("consumer offered the batch %d times, want 3", calls.Load())
	}
	m, err := dl.GetLastMsgForSubject(context.Background(), DeadLetterPrefix+".audit.broken")
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("W17-Dead-Letter-Subject") != "w17.requests.tasks.rest" || m.Header.Get("W17-Dead-Letter-Reason") == "" {
		t.Fatalf("headers %v", m.Header)
	}
	if b, err := DecodeBatch(m.Subject, m.Data, 0); err != nil || len(b.Records) != 2 {
		t.Fatalf("dead letter does not hold the batch: %v", err)
	}
}

func TestConsumers_Registration(t *testing.T) {
	cs := &Consumers{}
	c := funcConsumer{}
	if err := cs.Audit("bad name", c); err == nil {
		t.Error("a name with a space was accepted")
	}
	if err := cs.Audit("ok", c, "w17.events.tasks.>"); err == nil {
		t.Error("a subject outside the request stream was accepted")
	}
	if err := cs.Audit("ok", c); err != nil {
		t.Fatal(err)
	}
	if err := cs.Audit("ok", c); err == nil {
		t.Error("the same audit consumer registered twice")
	}
	if err := cs.Marketing("ok", c); err != nil {
		t.Errorf("one name for both purposes: %v", err)
	}
	if cs.Len() != 2 {
		t.Fatalf("Len = %d", cs.Len())
	}
}

// A record whose method this binary does not link keeps its bytes.
func TestDecodeBatch_UnlinkedTypesKeepTheirBytes(t *testing.T) {
	meta := &w17pb.RequestEventMeta{Method: "/acme.Unknown/Do", RequestField: 3, Purposes: []w17pb.RequestPurpose{w17pb.RequestPurpose_REQUEST_PURPOSE_AUDIT}}
	mb, _ := proto.Marshal(meta)
	var rec []byte
	rec = appendField(rec, 3, []byte{0x0a, 0x01, 'x'})
	rec = appendField(rec, EnvelopeMetaField, mb)
	wire := appendField(nil, BatchFieldRecords, rec)
	b, err := DecodeBatch("w17.requests.x.rest", wire, w17pb.RequestPurpose_REQUEST_PURPOSE_AUDIT)
	if err != nil || len(b.Records) != 1 {
		t.Fatalf("%v %v", b, err)
	}
	if r := b.Records[0]; r.Request != nil || string(r.RequestRaw) != "\x0a\x01x" {
		t.Fatalf("record %+v", r)
	}
	if b, _ := DecodeBatch("s", wire, w17pb.RequestPurpose_REQUEST_PURPOSE_MARKETING); len(b.Records) != 0 {
		t.Fatal("an audit-only record reached marketing")
	}
	if _, err := DecodeBatch("s", appendField(nil, BatchFieldRecords, appendField(nil, 3, nil)), 0); err == nil {
		t.Fatal("a record without meta decoded")
	}
}

func appendField(dst []byte, num protowire.Number, v []byte) []byte {
	dst = protowire.AppendTag(dst, num, protowire.BytesType)
	return protowire.AppendBytes(dst, v)
}

func TestConsumers_ServeNeedsADSNAndStopsWithItsContext(t *testing.T) {
	cs := &Consumers{}
	if err := cs.Serve(context.Background(), "", ConsumerConfig{DurablePrefix: "p"}); err == nil {
		t.Fatal("no DSN accepted")
	}
	_ = cs.Audit("a", funcConsumer{audit: func(context.Context, *ConsumedBatch) error { return nil }})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := cs.Serve(ctx, "nats://127.0.0.1:1", ConsumerConfig{DurablePrefix: "p"}); err != nil {
		t.Fatalf("Serve with NATS down returned %v instead of waiting for it", err)
	}
}
