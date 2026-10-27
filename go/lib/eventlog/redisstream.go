package eventlog

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/dsnredact"
)

// The request stream on Redis Streams, for a project whose bus is Redis and
// runs no NATS. Same batches, same guarantees as the JetStream stream:
//
//   - a batch counts as delivered once it is stored, and one sent twice (a
//     retried publish) is stored once — the batch id is kept for the
//     duplicate window beside the stream, and the XADD skips an id it holds;
//   - each registered consumer has its own consumer group, so one failing
//     never redelivers to another; a batch is acknowledged when Consume
//     returned nil, offered again after the backoff when it returned an
//     error, and moved to the dead letter stream (and reported) after
//     MaxDeliver offers — and stays pending when that move fails;
//   - a batch with no record of the consumer's purpose, or on a subject the
//     consumer does not read, is acknowledged without calling it.
//
// One stream holds every domain's batches, as on JetStream; the subject is
// an entry field. The keys share the `{w17:requests}` hash tag, so the
// stream, its dead letters and the duplicate-window markers sit in one hash
// slot and the publish script stays atomic on a cluster.

// Redis keys of the request stream.
const (
	RedisStreamKey     = "{w17:requests}"
	RedisDeadLetterKey = "{w17:requests}:dlq"
	redisBatchSeenKey  = "{w17:requests}:batch:"
)

// Entry fields of a batch on the Redis stream.
const (
	redisFieldSubject = "subject"
	redisFieldID      = "id"
	redisFieldOrgs    = "orgs"
	redisFieldWire    = "wire"
	redisFieldReason  = "reason"
	redisFieldFrom    = "consumer"
)

// RedisBatchBytes is the batch size a Redis-backed recorder aims at: Redis
// takes far larger values, but a batch is held whole by every consumer that
// reads it, as on NATS.
const RedisBatchBytes = 1 << 20

// redisPublish stores a batch once: when the duplicate-window marker for
// its id is absent, it XADDs the entry, trimming entries older than the
// stream's max age, and only then sets the marker; when the marker exists
// the batch was stored already. The marker is set after the XADD because a
// script is not rolled back: one set before an XADD that failed (maxmemory,
// say) would make every retry within the window skip a batch never stored.
// KEYS[1] stream, KEYS[2] marker; ARGV[1] window seconds, ARGV[2] trim
// MINID ("0" = none), then field/value pairs.
var redisPublish = goredis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then
  return false
end
local args = {'XADD', KEYS[1]}
if ARGV[2] ~= '0' then
  table.insert(args, 'MINID'); table.insert(args, '~'); table.insert(args, ARGV[2])
end
table.insert(args, '*')
for i = 3, #ARGV do table.insert(args, ARGV[i]) end
local id = redis.call(unpack(args))
redis.call('SET', KEYS[2], '1', 'EX', ARGV[1])
return id
`)

// RedisSink stores each batch on the Redis request stream, under the subject
// a JetStream sink would publish on.
type RedisSink struct {
	Client  goredis.UniversalClient
	Subject string
	Stream  StreamLimits
	// Timeout bounds one publish. Default 5s.
	Timeout time.Duration
	// now is the clock (tests).
	now func() time.Time
}

// Sink is a [Config.Sink].
func (s *RedisSink) Sink(ctx context.Context, b *Batch) error {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	l := redisLimits(s.Stream)
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	minID := "0"
	if l.MaxAge > 0 {
		minID = strconv.FormatInt(now().Add(-l.MaxAge).UnixMilli(), 10)
	}
	orgs := ""
	switch {
	case b.TagsOverflow:
		orgs = "*"
	case len(b.Tags) > 0:
		orgs = strings.Join(b.Tags, ",")
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := redisPublish.Run(pctx, s.Client,
		[]string{RedisStreamKey, redisBatchSeenKey + b.ID.String()},
		int64(l.Duplicates/time.Second), minID,
		redisFieldSubject, s.Subject, redisFieldID, b.ID.String(), redisFieldOrgs, orgs, redisFieldWire, b.Wire,
	).Err()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return fmt.Errorf("eventlog: publish to %s on redis: %w", s.Subject, err)
	}
	return nil
}

// redisLimits fills the defaults [EnsureStream] applies, for what Redis can
// honour: the age bound and the duplicate window. A Redis stream has no
// byte cap — the server's maxmemory is it — and no replica count of its own.
func redisLimits(l StreamLimits) StreamLimits {
	if l.MaxAge <= 0 {
		l.MaxAge = 7 * 24 * time.Hour
	}
	if l.Duplicates <= 0 {
		l.Duplicates = 2 * time.Minute
	}
	if l.Duplicates < time.Second {
		l.Duplicates = time.Second
	}
	return l
}

// IsRedisDSN reports whether dsn names a Redis server (redis:// or
// rediss://) rather than NATS: [Open] and [Consumers.Serve] pick the
// stream's backend by it.
func IsRedisDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "redis://") || strings.HasPrefix(dsn, "rediss://")
}

func dialRedis(ctx context.Context, dsn string) (*goredis.Client, error) {
	opt, err := goredis.ParseURL(dsn)
	if err != nil {
		return nil, fmt.Errorf("eventlog: redis DSN %q: %w", dsnredact.String(dsn), dsnredact.Err(err))
	}
	// go-redis ignores a context deadline unless told otherwise; the sink's
	// Timeout would bound nothing.
	opt.ContextTimeoutEnabled = true
	c := goredis.NewClient(opt)
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := c.Ping(pctx).Err(); err != nil {
		// Not fatal, as NATS not up yet is not: the client reconnects on
		// the next command, and a publish that fails takes the failure path.
		log.Printf("eventlog redis: %s not reachable yet (%v) — request records take the failure path until it is", dsnredact.String(dsn), err)
	}
	return c, nil
}

// runRedis is [Consumers.Run] on the Redis stream.
func (cs *Consumers) runRedis(ctx context.Context, cfg ConsumerConfig, list []registered) error {
	consumer := cfg.consumerName()
	// Every group first, and only then the loops: a group that cannot be
	// created fails Run with none started, so the caller's retry (serveLoop)
	// does not start a second set of loops beside ones still running.
	rcs := make([]*redisConsumer, 0, len(list))
	for _, r := range list {
		purpose := purposeName(r.purpose)
		group := cfg.DurablePrefix + "-" + purpose + "-" + r.name
		if err := ensureRedisGroup(ctx, cfg.Redis, group); err != nil {
			return err
		}
		rcs = append(rcs, &redisConsumer{cfg: cfg, r: r, group: group, consumer: consumer, from: purpose + "." + r.name, inflight: map[string]struct{}{}})
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2*len(rcs)) // a read and a claim loop each
	for _, rc := range rcs {
		wg.Add(2)
		go func() { defer wg.Done(); errs <- rc.readLoop(ctx) }()
		go func() { defer wg.Done(); errs <- rc.claimLoop(ctx) }()
	}
	wg.Wait()
	close(errs)
	var all []error
	for e := range errs {
		if e != nil {
			all = append(all, e)
		}
	}
	return errors.Join(all...)
}

// ensureRedisGroup creates the consumer group (and the stream, when no
// gateway has recorded yet), reading from the stream's start; one that
// exists already is kept as it is.
func ensureRedisGroup(ctx context.Context, c goredis.UniversalClient, group string) error {
	err := c.XGroupCreateMkStream(ctx, RedisStreamKey, group, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("eventlog: consumer group %s: %w", group, err)
	}
	return nil
}

func (cfg ConsumerConfig) consumerName() string {
	if cfg.ConsumerName != "" {
		return cfg.ConsumerName
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "w17-request-consumers"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}

func purposeName(p interface{ String() string }) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "REQUEST_PURPOSE_"))
}

type redisConsumer struct {
	cfg      ConsumerConfig
	r        registered
	group    string
	consumer string
	from     string // the dead letter's consumer field: <purpose>.<name>

	// serve makes the read and the claim loop offer one batch at a time, as
	// a JetStream consumer's single fetch loop does.
	serve sync.Mutex
	// inflight holds the entries this process has read or claimed and not
	// finished: pending in the group, but not failed, so claimLoop must not
	// offer them again.
	mu       sync.Mutex
	inflight map[string]struct{}
}

func (rc *redisConsumer) markInflight(ids ...string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, id := range ids {
		rc.inflight[id] = struct{}{}
	}
}

func (rc *redisConsumer) doneInflight(id string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.inflight, id)
}

func (rc *redisConsumer) isInflight(id string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	_, ok := rc.inflight[id]
	return ok
}

// offer hands one entry to handle, holding it in flight meanwhile.
func (rc *redisConsumer) offer(ctx context.Context, m goredis.XMessage, attempt int) {
	rc.serve.Lock()
	defer rc.serve.Unlock()
	defer rc.doneInflight(m.ID)
	rc.handle(ctx, m, attempt)
}

// readLoop takes the batches no consumer of the group has seen.
func (rc *redisConsumer) readLoop(ctx context.Context) error {
	for ctx.Err() == nil {
		res, err := rc.cfg.Redis.XReadGroup(ctx, &goredis.XReadGroupArgs{
			Group: rc.group, Consumer: rc.consumer, Streams: []string{RedisStreamKey, ">"},
			Count: 16, Block: time.Second,
		}).Result()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, goredis.Nil) {
				continue
			}
			if strings.HasPrefix(err.Error(), "NOGROUP") {
				// The stream or the group went away under a running
				// consumer (a Redis that restarted without its data, a
				// deleted key): the gateway's next record recreates the
				// stream, but nothing recreates the group, and every read
				// would fail on it for good. Create it again.
				log.Printf("eventlog: consumer %s: group %s is gone, creating it again", rc.r.name, rc.group)
				cerr := ensureRedisGroup(ctx, rc.cfg.Redis, rc.group)
				if cerr == nil {
					continue
				}
				if ctx.Err() == nil {
					log.Printf("eventlog: consumer %s: %v", rc.r.name, cerr)
				}
			} else {
				log.Printf("eventlog: consumer %s: read: %v", rc.r.name, err)
			}
			sleepCtx(ctx, time.Second)
			continue
		}
		// The whole read is in flight at once: the entries after the first
		// sit pending, idle, while the ones before them are served.
		for _, s := range res {
			for _, m := range s.Messages {
				rc.markInflight(m.ID)
			}
		}
		for _, s := range res {
			for _, m := range s.Messages {
				rc.offer(ctx, m, 1)
			}
		}
	}
	return nil
}

// claimLoop offers a failed batch again once its backoff has passed — the
// entry stays pending in the group until it is acknowledged — and takes over
// the batches a consumer that went away left pending.
func (rc *redisConsumer) claimLoop(ctx context.Context) error {
	for ctx.Err() == nil {
		sleepCtx(ctx, rc.cfg.claimEvery())
		if ctx.Err() != nil {
			return nil
		}
		pending, err := rc.cfg.Redis.XPendingExt(ctx, &goredis.XPendingExtArgs{
			Stream: RedisStreamKey, Group: rc.group, Start: "-", End: "+", Count: 64,
		}).Result()
		if err != nil {
			if !errors.Is(err, goredis.Nil) && ctx.Err() == nil {
				log.Printf("eventlog: consumer %s: pending: %v", rc.r.name, err)
			}
			continue
		}
		for _, p := range pending {
			// An entry this process is still serving is pending but not
			// failed: offering it again would run it twice, side by side.
			if rc.isInflight(p.ID) {
				continue
			}
			wait := rc.cfg.Backoff(int(p.RetryCount))
			if p.Consumer != rc.consumer {
				// Another process's entry may be in flight there: take it
				// over only once a Consume would have timed out (AckWait), as
				// JetStream redelivers an unacknowledged message.
				wait = max(wait, rc.cfg.AckWait)
			}
			if p.Idle < wait {
				continue
			}
			// MinIdle makes the claim conditional: another replica that
			// claimed the entry meanwhile reset its idle time.
			msgs, err := rc.cfg.Redis.XClaim(ctx, &goredis.XClaimArgs{
				Stream: RedisStreamKey, Group: rc.group, Consumer: rc.consumer,
				MinIdle: wait, Messages: []string{p.ID},
			}).Result()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("eventlog: consumer %s: claim %s: %v", rc.r.name, p.ID, err)
				}
				continue
			}
			for _, m := range msgs {
				rc.markInflight(m.ID)
				rc.offer(ctx, m, int(p.RetryCount)+1)
			}
		}
	}
	return nil
}

func (rc *redisConsumer) handle(ctx context.Context, m goredis.XMessage, attempt int) {
	subject, _ := m.Values[redisFieldSubject].(string)
	if !subjectMatchesAny(subject, rc.r.subjects) {
		rc.ack(ctx, m.ID)
		return
	}
	wire, _ := m.Values[redisFieldWire].(string)
	b, err := DecodeBatch(subject, []byte(wire), rc.r.purpose)
	if err == nil {
		if id, perr := uuid.Parse(fmt.Sprint(m.Values[redisFieldID])); perr == nil && b.ID == (uuid.UUID{}) {
			b.ID = id
		}
	}
	if err != nil {
		// A batch that does not decode never will: dead-letter it now.
		rc.deadLetter(ctx, m, subject, err)
		return
	}
	if len(b.Records) == 0 {
		rc.ack(ctx, m.ID)
		return
	}
	cerr := callConsumer(ctx, rc.r, b, rc.cfg.AckWait)
	if cerr == nil {
		rc.ack(ctx, m.ID)
		return
	}
	if attempt >= rc.cfg.MaxDeliver {
		rc.deadLetter(ctx, m, subject, cerr)
		return
	}
	// Left pending: claimLoop offers it again after Backoff(attempt).
}

func (rc *redisConsumer) ack(ctx context.Context, id string) {
	if err := rc.cfg.Redis.XAck(ctx, RedisStreamKey, rc.group, id).Err(); err != nil && ctx.Err() == nil {
		log.Printf("eventlog: consumer %s: ack %s: %v", rc.r.name, id, err)
	}
}

func (rc *redisConsumer) deadLetter(ctx context.Context, m goredis.XMessage, subject string, cause error) {
	values := map[string]any{redisFieldReason: cause.Error(), redisFieldFrom: rc.from}
	for k, v := range m.Values {
		values[k] = v
	}
	values[redisFieldSubject] = subject
	l := redisLimits(rc.cfg.Stream)
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := rc.cfg.Redis.XAdd(pctx, &goredis.XAddArgs{
		Stream: RedisDeadLetterKey,
		MinID:  strconv.FormatInt(time.Now().Add(-l.MaxAge).UnixMilli(), 10),
		Approx: true,
		Values: values,
	}).Err()
	if err != nil {
		// Not stored: leave it pending, to be offered again, rather than
		// lose it.
		observx.ReportError(ctx, fmt.Errorf("eventlog: consumer %s: dead-lettering a batch failed (%w) after: %w", rc.r.name, err, cause))
		return
	}
	rc.ack(ctx, m.ID)
	observx.ReportError(ctx, fmt.Errorf("eventlog: consumer %s: a batch moved to %s: %w", rc.r.name, RedisDeadLetterKey, cause))
}

func (cfg ConsumerConfig) claimEvery() time.Duration {
	if cfg.ClaimInterval > 0 {
		return cfg.ClaimInterval
	}
	return time.Second
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// subjectMatchesAny matches a subject against NATS-style filters: `*` is one
// token, a trailing `>` one or more — what a JetStream consumer's
// FilterSubjects would select.
func subjectMatchesAny(subject string, filters []string) bool {
	for _, f := range filters {
		if subjectMatches(subject, f) {
			return true
		}
	}
	return false
}

func subjectMatches(subject, filter string) bool {
	st, ft := strings.Split(subject, "."), strings.Split(filter, ".")
	for i, f := range ft {
		if f == ">" {
			return len(st) > i
		}
		if i >= len(st) || (f != "*" && f != st[i]) {
			return false
		}
	}
	return len(st) == len(ft)
}
