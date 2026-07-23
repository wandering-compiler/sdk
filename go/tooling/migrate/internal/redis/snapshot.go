package redis

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Snapshotter is the Redis dump/restore driver for the dev DB
// lifecycle. It uses Redis's own per-key serialization (`DUMP` /
// `RESTORE`) over the native go-redis client — no `redis-cli` dep —
// which round-trips every value type (string, hash, list, set, zset,
// stream, …) byte-for-byte along with its TTL. The dump is a gob
// stream of one record per key; Restore replays each via
// `RESTORE … REPLACE` so it overwrites whatever the target holds.
//
// Scope = the single logical DB the DSN selects (the go-redis client
// is pinned to it). Cluster-wide / multi-DB snapshots are out of scope
// (dev runs a single-node Redis per the example stacks).
type Snapshotter struct {
	app *Applier
}

var _ migrate.Snapshotter = (*Snapshotter)(nil)

// snapRecord is one key's serialized form. TTLMillis is 0 for a key
// with no expiry (the value `RESTORE` reads as "persist").
type snapRecord struct {
	Key       string
	TTLMillis int64
	Payload   []byte
}

// NewSnapshotter reuses the Applier's DSN parsing + lazy client
// construction, so the snapshot path shares exactly the connection
// semantics of the apply path.
func NewSnapshotter(dsn string) (*Snapshotter, error) {
	app, err := New(context.Background(), dsn)
	if err != nil {
		return nil, err
	}
	return &Snapshotter{app: app}, nil
}

// Dump SCANs the whole keyspace and writes a gob record per key
// (`DUMP` payload + remaining TTL). SCAN is used rather than KEYS so
// a large dev keyspace doesn't block the server.
func (s *Snapshotter) Dump(ctx context.Context, w io.Writer) error {
	client := s.app.client
	enc := gob.NewEncoder(w)
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, "*", 512).Result()
		if err != nil {
			return fmt.Errorf("redis Dump: SCAN: %w", err)
		}
		for _, key := range keys {
			payload, err := client.Dump(ctx, key).Result()
			if err != nil {
				// Key vanished between SCAN and DUMP (TTL expiry); skip.
				if errors.Is(err, goredis.Nil) {
					continue
				}
				return fmt.Errorf("redis Dump: DUMP %q: %w", key, err)
			}
			ttl, err := client.PTTL(ctx, key).Result()
			if err != nil {
				return fmt.Errorf("redis Dump: PTTL %q: %w", key, err)
			}
			rec := snapRecord{Key: key, Payload: []byte(payload)}
			// PTTL: -1 = no expiry, -2 = no key (raced). Map both to 0
			// (RESTORE's "persist" / skip).
			if ttl > 0 {
				rec.TTLMillis = ttl.Milliseconds()
			}
			if err := enc.Encode(rec); err != nil {
				return fmt.Errorf("redis Dump: encode %q: %w", key, err)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return nil
}

// Restore reads the gob stream and RESTOREs each key with REPLACE, so
// it overwrites an existing key of the same name. It does NOT flush
// keys absent from the snapshot — the reconcile restores into a freshly
// wiped/built store, so the live keyspace is empty at restore time.
func (s *Snapshotter) Restore(ctx context.Context, r io.Reader) error {
	client := s.app.client
	dec := gob.NewDecoder(r)
	for {
		var rec snapRecord
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("redis Restore: decode: %w", err)
		}
		// go-redis RestoreReplace takes the TTL as a time.Duration; 0
		// means no expiry. Convert from the stored millis.
		if err := client.RestoreReplace(ctx, rec.Key, ttlDuration(rec.TTLMillis), string(rec.Payload)).Err(); err != nil {
			return fmt.Errorf("redis Restore: RESTORE %q: %w", rec.Key, err)
		}
	}
	return nil
}

// ttlDuration converts stored milliseconds back to the time.Duration
// go-redis RESTORE expects. 0 → 0 (no expiry).
func ttlDuration(millis int64) time.Duration {
	if millis <= 0 {
		return 0
	}
	return time.Duration(millis) * time.Millisecond
}
