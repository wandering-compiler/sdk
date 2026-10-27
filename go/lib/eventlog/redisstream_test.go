package eventlog

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

func redisFixture(t *testing.T) (*miniredis.Miniredis, *goredis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return mr, c
}

// recordTwoRedis records a signed-in and an anonymous request on the rest
// surface of domain tasks, through a recorder whose stream is Redis.
func recordTwoRedis(t *testing.T, c *goredis.Client) {
	t.Helper()
	r, err := NewRecorder(context.Background(), RecorderConfig{
		Domain:   "tasks",
		Redis:    c,
		Surfaces: map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Marketing: true, Audit: true}},
		Auth:     AuthFields{Scopes: scopesField, MarketingConsent: consentField},
	})
	if err != nil {
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

func runRedisConsumers(t *testing.T, cs *Consumers, c *goredis.Client, cfg ConsumerConfig) (stop func()) {
	t.Helper()
	cfg.Redis = c
	if cfg.DurablePrefix == "" {
		cfg.DurablePrefix = "test"
	}
	if cfg.ClaimInterval == 0 {
		cfg.ClaimInterval = 20 * time.Millisecond
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

// The batch lands on the Redis stream with what a JetStream message carries
// in its subject and headers: the subject, the batch id, the organizations.
func TestRedisSink_StoresTheBatchWithItsSubjectAndOrgs(t *testing.T) {
	_, c := redisFixture(t)
	recordTwoRedis(t, c)
	entries, err := c.XRange(context.Background(), RedisStreamKey, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries, want one batch", len(entries))
	}
	v := entries[0].Values
	if v[redisFieldSubject] != "w17.requests.tasks.rest" || v[redisFieldOrgs] != "org-1" {
		t.Fatalf("entry %v", v)
	}
	if _, err := uuid.Parse(v[redisFieldID].(string)); err != nil {
		t.Fatalf("batch id %v: %v", v[redisFieldID], err)
	}
	if b, err := DecodeBatch("w17.requests.tasks.rest", []byte(v[redisFieldWire].(string)), 0); err != nil || len(b.Records) != 2 {
		t.Fatalf("the stored wire is not the batch: %v", err)
	}
}

// A publish retried after it was stored (a lost reply) stores nothing new:
// the batch id is held for the duplicate window, as JetStream's Nats-Msg-Id.
func TestRedisSink_ARetriedBatchIsStoredOnce(t *testing.T) {
	mr, c := redisFixture(t)
	s := &RedisSink{Client: c, Subject: "w17.requests.tasks.rest", Stream: StreamLimits{Duplicates: time.Minute}}
	b := &Batch{ID: uuid.New(), Wire: []byte("x")}
	for range 2 {
		if err := s.Sink(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := c.XLen(context.Background(), RedisStreamKey).Result(); n != 1 {
		t.Fatalf("%d entries after a retried publish, want 1", n)
	}
	mr.FastForward(2 * time.Minute)
	if err := s.Sink(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.XLen(context.Background(), RedisStreamKey).Result(); n != 2 {
		t.Fatalf("past the window the id is new again: %d entries, want 2", n)
	}
}

// Entries older than the stream's max age are trimmed on publish.
func TestRedisSink_TrimsPastMaxAge(t *testing.T) {
	_, c := redisFixture(t)
	old := time.Now().Add(-48 * time.Hour)
	s := &RedisSink{Client: c, Subject: "w17.requests.tasks.rest", Stream: StreamLimits{MaxAge: 24 * time.Hour}}
	if err := c.XAdd(context.Background(), &goredis.XAddArgs{Stream: RedisStreamKey, ID: formatMs(old), Values: map[string]any{"wire": "old"}}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.Sink(context.Background(), &Batch{ID: uuid.New(), Wire: []byte("new")}); err != nil {
		t.Fatal(err)
	}
	entries, _ := c.XRange(context.Background(), RedisStreamKey, "-", "+").Result()
	if len(entries) != 1 || entries[0].Values[redisFieldWire] != "new" {
		t.Fatalf("entries %v, want only the new one", entries)
	}
}

func formatMs(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) + "-0" }

// Each consumer reads its own purpose's records, decoded with the method's
// own types; a marketing consumer that fails is offered the batch again
// after its backoff without the audit consumer seeing it twice.
func TestRedisConsumers_EachItsPurposeAndItsOwnProgress(t *testing.T) {
	_, c := redisFixture(t)
	recordTwoRedis(t, c)

	var mu sync.Mutex
	var marketing, audit [][]ConsumedRecord
	var mCalls, aCalls atomic.Int32
	cs := &Consumers{}
	fc := funcConsumer{
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
	if err := cs.Marketing("crm", fc); err != nil {
		t.Fatal(err)
	}
	if err := cs.Audit("trail", fc, "w17.requests.tasks.>"); err != nil {
		t.Fatal(err)
	}
	stop := runRedisConsumers(t, cs, c, ConsumerConfig{Backoff: func(int) time.Duration { return 50 * time.Millisecond }})
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(marketing) == 1 && len(audit) == 1 })
	stop()

	if aCalls.Load() != 1 || mCalls.Load() != 2 {
		t.Fatalf("audit called %d times, marketing %d (want 1 and 2)", aCalls.Load(), mCalls.Load())
	}
	if len(audit[0]) != 2 || len(marketing[0]) != 1 {
		t.Fatalf("audit got %d records, marketing %d (want 2 and 1)", len(audit[0]), len(marketing[0]))
	}
	m := marketing[0][0]
	if req, ok := m.Request.(*healthpb.HealthCheckRequest); !ok || req.GetService() != "tasks" || m.Meta.GetUserId() != "u-1" {
		t.Fatalf("marketing record %+v", m)
	}
	// Both are acknowledged: nothing is left pending in either group.
	for _, g := range []string{"test-marketing-crm", "test-audit-trail"} {
		p, err := c.XPending(context.Background(), RedisStreamKey, g).Result()
		if err != nil || p.Count != 0 {
			t.Errorf("group %s: pending %v (%v), want none", g, p, err)
		}
	}
}

// A Redis that loses its data under running consumers (restarted without
// persistence) takes the groups with it: the gateway's next record recreates
// the stream, and the consumer recreates its group rather than failing every
// read on it from then on.
func TestRedisConsumers_AGroupThatWentAwayIsCreatedAgain(t *testing.T) {
	mr, c := redisFixture(t)
	var calls atomic.Int32
	cs := &Consumers{}
	if err := cs.Audit("trail", funcConsumer{audit: func(context.Context, *ConsumedBatch) error {
		calls.Add(1)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	stop := runRedisConsumers(t, cs, c, ConsumerConfig{})
	defer stop()
	recordTwoRedis(t, c)
	waitUntil(t, func() bool { return calls.Load() == 1 })
	mr.FlushAll()
	recordTwoRedis(t, c)
	waitUntil(t, func() bool { return calls.Load() == 2 })
}

// A consumer reading another domain's subject acknowledges the batch without
// being called, as a JetStream filter would never deliver it.
func TestRedisConsumers_AnotherSubjectIsNotOffered(t *testing.T) {
	_, c := redisFixture(t)
	recordTwoRedis(t, c)
	var calls atomic.Int32
	cs := &Consumers{}
	_ = cs.Audit("other", funcConsumer{audit: func(context.Context, *ConsumedBatch) error { calls.Add(1); return nil }}, "w17.requests.billing.>")
	stop := runRedisConsumers(t, cs, c, ConsumerConfig{})
	entries, _ := c.XRange(context.Background(), RedisStreamKey, "-", "+").Result()
	waitUntil(t, func() bool {
		p, err := c.XPending(context.Background(), RedisStreamKey, "test-audit-other").Result()
		info, ierr := c.XInfoGroups(context.Background(), RedisStreamKey).Result()
		return err == nil && ierr == nil && p.Count == 0 && len(info) == 1 && info[0].LastDeliveredID == entries[0].ID
	})
	stop()
	if calls.Load() != 0 {
		t.Fatalf("a consumer of billing was offered a tasks batch %d times", calls.Load())
	}
}

// A batch its consumer keeps failing goes to the dead letter stream after
// MaxDeliver offers, with the reason and the subject — never lost, never
// offered forever.
func TestRedisConsumers_DeadLetterAfterMaxDeliver(t *testing.T) {
	_, c := redisFixture(t)
	recordTwoRedis(t, c)
	var calls atomic.Int32
	cs := &Consumers{}
	_ = cs.Audit("broken", funcConsumer{audit: func(context.Context, *ConsumedBatch) error {
		calls.Add(1)
		panic("consumer bug")
	}})
	stop := runRedisConsumers(t, cs, c, ConsumerConfig{MaxDeliver: 3, Backoff: func(int) time.Duration { return 20 * time.Millisecond }})
	waitUntil(t, func() bool { n, _ := c.XLen(context.Background(), RedisDeadLetterKey).Result(); return n == 1 })
	stop()
	if calls.Load() != 3 {
		t.Fatalf("consumer offered the batch %d times, want 3", calls.Load())
	}
	dl, _ := c.XRange(context.Background(), RedisDeadLetterKey, "-", "+").Result()
	v := dl[0].Values
	if v[redisFieldSubject] != "w17.requests.tasks.rest" || v[redisFieldFrom] != "audit.broken" || v[redisFieldReason] == "" {
		t.Fatalf("dead letter %v", v)
	}
	if b, err := DecodeBatch("w17.requests.tasks.rest", []byte(v[redisFieldWire].(string)), 0); err != nil || len(b.Records) != 2 {
		t.Fatalf("dead letter does not hold the batch: %v", err)
	}
	if p, _ := c.XPending(context.Background(), RedisStreamKey, "test-audit-broken").Result(); p.Count != 0 {
		t.Fatalf("a dead-lettered batch is still pending: %v", p)
	}
}

// A batch still being served is pending in its group but has not failed:
// the claim loop leaves it alone however short the backoff, so a slow
// Consume is not run a second time beside itself.
func TestRedisConsumers_ABatchInFlightIsNotOfferedAgain(t *testing.T) {
	_, c := redisFixture(t)
	recordTwoRedis(t, c)
	var calls, running, overlap atomic.Int32
	cs := &Consumers{}
	_ = cs.Audit("slow", funcConsumer{audit: func(context.Context, *ConsumedBatch) error {
		calls.Add(1)
		if running.Add(1) > 1 {
			overlap.Add(1)
		}
		defer running.Add(-1)
		time.Sleep(300 * time.Millisecond)
		return nil
	}})
	stop := runRedisConsumers(t, cs, c, ConsumerConfig{Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
	waitUntil(t, func() bool {
		p, err := c.XPending(context.Background(), RedisStreamKey, "test-audit-slow").Result()
		return calls.Load() >= 1 && err == nil && p.Count == 0
	})
	stop()
	if calls.Load() != 1 || overlap.Load() != 0 {
		t.Fatalf("a batch in flight was offered %d times (%d side by side), want once", calls.Load(), overlap.Load())
	}
}

// Open and Serve pick the backend by the DSN's scheme: a redis:// DSN
// records to and consumes from the Redis stream.
func TestRedisDSN_OpenAndServe(t *testing.T) {
	mr, c := redisFixture(t)
	dsn := "redis://" + mr.Addr()
	r, err := Open(context.Background(), dsn, RecorderConfig{
		Domain:   "tasks",
		Surfaces: map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := r.Surface(w17pb.RequestApi_REQUEST_API_REST).Start(healthMethod)
	rec.SetRequest(&healthpb.HealthCheckRequest{Service: "x"})
	rec.Finish(context.Background(), 200)
	closeRecorder(t, r)

	var got atomic.Int32
	cs := &Consumers{}
	_ = cs.Audit("a", funcConsumer{audit: func(_ context.Context, b *ConsumedBatch) error { got.Add(int32(len(b.Records))); return nil }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cs.Serve(ctx, dsn, ConsumerConfig{DurablePrefix: "p"}) }()
	waitUntil(t, func() bool { return got.Load() == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !IsRedisDSN("rediss://h:6380/0") || IsRedisDSN("nats://h:4222") {
		t.Error("IsRedisDSN")
	}
	_ = c
}

func TestSubjectMatches(t *testing.T) {
	for _, c := range []struct {
		subject, filter string
		want            bool
	}{
		{"w17.requests.tasks.rest", "w17.requests.>", true},
		{"w17.requests.tasks.rest", "w17.requests.tasks.>", true},
		{"w17.requests.tasks.rest", "w17.requests.*.rest", true},
		{"w17.requests.tasks.rest", "w17.requests.billing.>", false},
		{"w17.requests.tasks.rest", "w17.requests.tasks.rest", true},
		{"w17.requests.tasks", "w17.requests.tasks.>", false},
		{"w17.requests.tasks.rest", "w17.requests.*", false},
	} {
		if got := subjectMatches(c.subject, c.filter); got != c.want {
			t.Errorf("subjectMatches(%q, %q) = %v", c.subject, c.filter, got)
		}
	}
}
