package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	goredis "github.com/redis/go-redis/v9"
)

// Settling an event's handlers one by one — the arm ledger.
//
// A surface's consumer receives each message of a channel once, and when the
// surface subscribes several handlers to one event, the generated dispatcher
// runs all of them (its "arms") on that one delivery. The delivery's outcome
// is the whole set's: one failing arm Naks the message, and on the
// redelivery every arm ran again — an arm that had already succeeded
// included. A non-idempotent handler (a counter) then applied its effect
// once per redelivery, with every call reporting success.
//
// The ledger is how the dispatcher settles arms separately. After an attempt
// that failed, it records the arms that succeeded on that delivery
// (RecordSettledArms); on a redelivery it skips them (SettledArms). The
// record lives in the broker — a JetStream key-value bucket, a Redis set — so
// it holds whichever replica the redelivery reaches. It is best effort: a
// record that cannot be written or read runs the arm again, which is the
// at-least-once delivery the bus promises anyway.
//
// A delivery that succeeds is never redelivered, so neither call touches the
// broker on the happy path: SettledArms reads only on a redelivery, and the
// dispatcher records only when an attempt failed.

// armLedger is one delivery's record.
type armLedger interface {
	load(ctx context.Context) (map[string]bool, error)
	store(ctx context.Context, arms []string) error
}

type armLedgerKey struct{}

type deliveryArms struct {
	ledger      armLedger
	redelivered bool
	channel     string
	topic       string
}

// withArmLedger attaches a delivery's ledger to the handler's ctx.
func withArmLedger(ctx context.Context, l armLedger, redelivered bool, channel, topic string) context.Context {
	return context.WithValue(ctx, armLedgerKey{}, &deliveryArms{ledger: l, redelivered: redelivered, channel: channel, topic: topic})
}

// SettledArms returns the arms an earlier attempt of the delivery ctx
// carries already completed, keyed by the name the dispatcher recorded them
// under. Nil on a first delivery, on a bus that keeps no ledger (MemoryBus,
// which never redelivers), and when the record cannot be read — the arms
// then run again.
func SettledArms(ctx context.Context) map[string]bool {
	d, _ := ctx.Value(armLedgerKey{}).(*deliveryArms)
	if d == nil || !d.redelivered {
		return nil
	}
	got, err := d.ledger.load(ctx)
	if err != nil {
		reportBusFailure("arm ledger read", d.channel, d.topic, err)
		return nil
	}
	return got
}

// RecordSettledArms records arms as completed on the delivery ctx carries,
// so its redelivery skips them. The dispatcher calls it when an attempt
// failed; the arms already on record stay there. A failure to record is
// reported and otherwise ignored (the arms run again on the redelivery).
func RecordSettledArms(ctx context.Context, arms ...string) {
	d, _ := ctx.Value(armLedgerKey{}).(*deliveryArms)
	if d == nil || len(arms) == 0 {
		return
	}
	// The attempt's own deadline may be what failed it; the record must
	// still land.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), armLedgerTimeout)
	defer cancel()
	if err := d.ledger.store(sctx, arms); err != nil {
		reportBusFailure("arm ledger write", d.channel, d.topic, err)
	}
}

// armLedgerTTL is how long a delivery's record is kept: a channel stream's
// default retention, past which the message itself is gone.
const armLedgerTTL = DefaultStreamMaxAge

// armLedgerTimeout bounds one ledger write.
const armLedgerTimeout = 5 * time.Second

func encodeArms(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

func decodeArms(v string) map[string]bool {
	out := map[string]bool{}
	for _, n := range strings.Split(v, "\n") {
		if n != "" {
			out[n] = true
		}
	}
	return out
}

// --- NATS: one key per delivery in a key-value bucket.

const natsArmBucket = "w17_eventbus_arms"

type natsArmLedger struct {
	bus *NatsBus
	key string
}

func natsArmKey(channel, consumer string, seq uint64) string {
	return channel + "." + consumer + "." + strconv.FormatUint(seq, 10)
}

func (b *NatsBus) armBucket(ctx context.Context, create bool) (jetstream.KeyValue, error) {
	b.armsMu.Lock()
	kv := b.arms
	b.armsMu.Unlock()
	if kv != nil {
		return kv, nil
	}
	var err error
	if create {
		kv, err = b.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:      natsArmBucket,
			TTL:         armLedgerTTL,
			Description: "w17 eventbus: the handlers of a delivery that already completed, so its redelivery skips them",
		})
	} else {
		kv, err = b.js.KeyValue(ctx, natsArmBucket)
	}
	if err != nil {
		return nil, err
	}
	b.armsMu.Lock()
	b.arms = kv
	b.armsMu.Unlock()
	return kv, nil
}

func (l natsArmLedger) load(ctx context.Context) (map[string]bool, error) {
	kv, err := l.bus.armBucket(ctx, false)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e, err := kv.Get(ctx, l.key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeArms(string(e.Value())), nil
}

func (l natsArmLedger) store(ctx context.Context, arms []string) error {
	kv, err := l.bus.armBucket(ctx, true)
	if err != nil {
		return fmt.Errorf("arm bucket %q: %w", natsArmBucket, err)
	}
	set, err := l.load(ctx)
	if err != nil {
		return err
	}
	if set == nil {
		set = map[string]bool{}
	}
	for _, a := range arms {
		set[a] = true
	}
	_, err = kv.Put(ctx, l.key, []byte(encodeArms(set)))
	return err
}

// --- Redis: one set per delivery.

type redisArmLedger struct {
	client goredis.UniversalClient
	key    string
}

func redisArmKey(channel, group, id string) string {
	return "w17:eventbus:arms:" + channel + ":" + group + ":" + id
}

func (l redisArmLedger) load(ctx context.Context) (map[string]bool, error) {
	names, err := l.client.SMembers(ctx, l.key).Result()
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func (l redisArmLedger) store(ctx context.Context, arms []string) error {
	members := make([]any, len(arms))
	for i, a := range arms {
		members[i] = a
	}
	_, err := l.client.TxPipelined(ctx, func(p goredis.Pipeliner) error {
		p.SAdd(ctx, l.key, members...)
		p.Expire(ctx, l.key, armLedgerTTL)
		return nil
	})
	return err
}
