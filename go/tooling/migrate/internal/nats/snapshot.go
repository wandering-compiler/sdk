package nats

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Snapshotter is the NATS dump/restore driver for the dev DB
// lifecycle. It snapshots **JetStream KV buckets** — the durable
// key/value state w17 services keep on NATS — over the native client,
// reusing the Applier's lazy connection. Dump writes a gob stream:
// one bucket-marker record per KV store (so empty buckets are
// recreated) followed by one record per key holding its latest value.
// Restore recreates each bucket (tolerating pre-existing) and Puts the
// keys back.
//
// **Deliberately lossy** (documented in the spec's S2 notes; acceptable
// for disposable dev scratch):
//   - Only the *latest* revision of each key is captured — KV history
//     and per-key TTLs are dropped.
//   - Bucket configuration (replicas, max-bytes, storage tier, …) is
//     not preserved; buckets are recreated with defaults.
//   - JetStream *streams* and their messages + consumer state are NOT
//     snapshotted. In w17 streams carry in-flight events (transport),
//     not primary state; losing them on a branch switch is expected.
type Snapshotter struct {
	app *Applier
}

var _ migrate.Snapshotter = (*Snapshotter)(nil)

// natsRecord is one entry in the gob stream. A bucket marker
// (IsBucket=true) carries only Bucket; a value record carries
// Bucket+Key+Value.
type natsRecord struct {
	Bucket   string
	Key      string
	Value    []byte
	IsBucket bool
}

// NewSnapshotter reuses the Applier constructor so DSN parsing +
// lazy-connect semantics match the apply path.
func NewSnapshotter(dsn string) (*Snapshotter, error) {
	app, err := New(context.Background(), dsn)
	if err != nil {
		return nil, err
	}
	return &Snapshotter{app: app}, nil
}

// Dump enumerates every KV bucket and writes its keys' latest values.
func (s *Snapshotter) Dump(ctx context.Context, w io.Writer) error {
	js, err := s.app.jsContext()
	if err != nil {
		return err
	}
	enc := gob.NewEncoder(w)

	names := js.KeyValueStoreNames(ctx)
	for name := range names.Name() {
		if err := enc.Encode(natsRecord{Bucket: name, IsBucket: true}); err != nil {
			return fmt.Errorf("nats Dump: encode bucket %q: %w", name, err)
		}
		kv, err := js.KeyValue(ctx, name)
		if err != nil {
			return fmt.Errorf("nats Dump: open bucket %q: %w", name, err)
		}
		keys, err := kv.Keys(ctx)
		if err != nil {
			if errors.Is(err, jetstream.ErrNoKeysFound) {
				continue // empty bucket — marker already written
			}
			return fmt.Errorf("nats Dump: keys %q: %w", name, err)
		}
		for _, key := range keys {
			entry, err := kv.Get(ctx, key)
			if err != nil {
				if errors.Is(err, jetstream.ErrKeyNotFound) {
					continue // raced delete
				}
				return fmt.Errorf("nats Dump: get %q/%q: %w", name, key, err)
			}
			if err := enc.Encode(natsRecord{Bucket: name, Key: key, Value: entry.Value()}); err != nil {
				return fmt.Errorf("nats Dump: encode %q/%q: %w", name, key, err)
			}
		}
	}
	if err := names.Error(); err != nil {
		return fmt.Errorf("nats Dump: list buckets: %w", err)
	}
	return nil
}

// Restore recreates each bucket and Puts its keys back.
func (s *Snapshotter) Restore(ctx context.Context, r io.Reader) error {
	js, err := s.app.jsContext()
	if err != nil {
		return err
	}
	dec := gob.NewDecoder(r)
	handles := map[string]jetstream.KeyValue{}
	for {
		var rec natsRecord
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("nats Restore: decode: %w", err)
		}
		if rec.IsBucket {
			kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: rec.Bucket})
			if err != nil {
				if errors.Is(err, jetstream.ErrBucketExists) {
					kv, err = js.KeyValue(ctx, rec.Bucket)
				}
				if err != nil {
					return fmt.Errorf("nats Restore: create bucket %q: %w", rec.Bucket, err)
				}
			}
			handles[rec.Bucket] = kv
			continue
		}
		kv, ok := handles[rec.Bucket]
		if !ok {
			// Value record before its bucket marker — shouldn't happen
			// for our own dumps, but bind defensively.
			kv, err = js.KeyValue(ctx, rec.Bucket)
			if err != nil {
				return fmt.Errorf("nats Restore: bind bucket %q: %w", rec.Bucket, err)
			}
			handles[rec.Bucket] = kv
		}
		if _, err := kv.Put(ctx, rec.Key, rec.Value); err != nil {
			return fmt.Errorf("nats Restore: put %q/%q: %w", rec.Bucket, rec.Key, err)
		}
	}
	return nil
}
