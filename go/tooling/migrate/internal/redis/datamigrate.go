package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/dmbuild"
)

// dataMigrationCursorPrefix is the Redis key prefix for the
// per-migration op-completion side-channel (Phase E v2.1
// resumability — D-iter3-15 §Resumability + D-iter3-17 follow-
// up). One Hash per migration id; fields are op-index strings
// ("0", "1", …); values are "complete".
//
// Cursor is consulted on each op start (HEXISTS skips the op if
// it's already been processed) and DEL'd after the migration's
// final bookkeeping write succeeds — so a successful end-to-end
// run leaves no cursor object behind.
const dataMigrationCursorPrefix = "wc:data-migrations:"

// dataRollbackCursorPrefix mirrors dataMigrationCursorPrefix for
// the down-direction. Forward + rollback cursors are separated
// so a partial-rollback re-runs against its own state without
// confusing the forward cursor for an already-rolled-forward
// migration.
const dataRollbackCursorPrefix = "wc:data-rollbacks:"

// applyYAMLDataMigration handles migration bodies whose `up_sql`
// is a YAML data migration (Phase E — D-iter3-15) instead of a
// redis-cli-style command script. Flow:
//
//  1. Parse YAML via lib/datamigrate.Unmarshal (rejects
//     malformed bodies + v2-only features the apply tool can't
//     execute).
//  2. For each Operation: SCAN the keyspace, fan keys out to a
//     pool of N worker goroutines, each running
//     GET→JSON-decode→mutate→JSON-encode→SET on a key.
//  3. After every op succeeds: write a cursor entry so a
//     subsequent re-apply (interrupted run) skips this op.
//  4. After every op succeeds: write the wc:migrations hash
//     entry for this migration's id (`HSET wc:migrations
//     <id> <hex>`). Bookkeeping is the apply tool's
//     responsibility for YAML bodies — applied.Wrap is
//     intentionally skipped at registry-side for YAML bodies
//     because Wrap injects command-shaped INSERT/HSET that
//     don't compose with a YAML doc.
//
// Each per-key update for the SCHEMA op kinds is replay-safe:
//   - ADD_FIELD_DEFAULT only sets the field when missing
//   - REMOVE_FIELD is naturally idempotent
//   - RENAME_FIELD only renames when From exists
//
// So for those a partial-failure restart re-runs the YAML body
// cheaply — the cursor skips already-completed ops at op-
// granularity, and within a half-completed op the per-key
// idempotency means re-running already-processed keys is a
// SCAN + GET + no-op SET (the changed flag stays false),
// avoiding the network write.
//
// TRANSFORM_FIELD is the exception: the user's Starlark script
// may be non-idempotent (e.g. `x = x * 2`, append-to-list), so
// re-running it on a key it already mutated double-applies. The
// op-granularity cursor can't cover that — a mid-op interruption
// would otherwise re-transform every key. We mark each
// TRANSFORM_FIELD op "started" before it mutates anything and
// REFUSE to silently resume one found started-but-not-complete,
// surfacing an operator warning instead of corrupting data (the
// operator restores/verifies state and clears the marker to
// retry). See guardTransformResume.
func (a *Applier) applyYAMLDataMigration(ctx context.Context, m *applyfetchpb.Migration) error {
	mig, err := datamigrate.Unmarshal([]byte(m.GetUpSql()))
	if err != nil {
		return fmt.Errorf("redis applyYAMLDataMigration: parse: %w", err)
	}
	codec, err := buildCodec(mig)
	if err != nil {
		return fmt.Errorf("redis applyYAMLDataMigration: %w", err)
	}
	vms, err := buildTransformVMs(mig)
	if err != nil {
		return fmt.Errorf("redis applyYAMLDataMigration: %w", err)
	}
	parallel := datamigrate.EffectiveParallel(mig, a.parallelOverride)
	cursorKey := dataMigrationCursorPrefix + m.GetId()
	for i, op := range mig.Operations {
		done, err := a.opAlreadyComplete(ctx, cursorKey, i)
		if err != nil {
			return fmt.Errorf("redis applyYAMLDataMigration: cursor read op[%d]: %w", i, err)
		}
		if done {
			continue
		}
		if err := a.guardTransformResume(ctx, cursorKey, i, op); err != nil {
			return fmt.Errorf("redis applyYAMLDataMigration: %w", err)
		}
		if err := a.runDataOp(ctx, op, i, parallel, codec, vms); err != nil {
			return fmt.Errorf("redis applyYAMLDataMigration: op[%d] %s on %s: %w",
				i, op.Op, op.Keyspace, err)
		}
		if err := a.markOpComplete(ctx, cursorKey, i); err != nil {
			return fmt.Errorf("redis applyYAMLDataMigration: cursor write op[%d]: %w", i, err)
		}
	}
	if err := a.recordMigrationApplied(ctx, m); err != nil {
		return fmt.Errorf("redis applyYAMLDataMigration: bookkeeping: %w", err)
	}
	if err := a.clearCursor(ctx, cursorKey); err != nil {
		return fmt.Errorf("redis applyYAMLDataMigration: cursor clear: %w", err)
	}
	return nil
}

// buildCodec / buildTransformVMs delegate to the shared dmbuild helpers; the
// build logic is backend-agnostic and lives in one place (see dmbuild).
func buildCodec(mig *datamigrate.Migration) (*datamigrate.ProtoCodec, error) {
	return dmbuild.Codec(mig)
}

func buildTransformVMs(mig *datamigrate.Migration) (map[int]*datamigrate.TransformVM, error) {
	return dmbuild.TransformVMs(mig)
}

// rollbackYAMLDataMigration is the inverse path. The down body
// is either:
//
//   - A YAML data migration (auto-derived inverse from
//     emit/redis) — same execution machinery as forward; the
//     bookkeeping write is replaced by HDEL.
//   - A `# wc:irreversible:` comment block (REMOVE_FIELD in
//     forward direction has no inverse). Refused: the operator
//     must explicitly `--allow-irreversible` to skip.
//
// Cursor side-channel uses dataRollbackCursorPrefix so a
// partial rollback resumes against its own state, independent
// of any forward-direction cursor that might still be present.
func (a *Applier) rollbackYAMLDataMigration(ctx context.Context, m *applyfetchpb.Migration) error {
	body := m.GetDownSql()
	if isIrreversibleMarkerBody(body) {
		return errors.New("redis rollbackYAMLDataMigration: down body is irreversible (REMOVE_FIELD has no inverse) — refused without --allow-irreversible")
	}
	mig, err := datamigrate.Unmarshal([]byte(body))
	if err != nil {
		return fmt.Errorf("redis rollbackYAMLDataMigration: parse: %w", err)
	}
	codec, err := buildCodec(mig)
	if err != nil {
		return fmt.Errorf("redis rollbackYAMLDataMigration: %w", err)
	}
	vms, err := buildTransformVMs(mig)
	if err != nil {
		return fmt.Errorf("redis rollbackYAMLDataMigration: %w", err)
	}
	parallel := datamigrate.EffectiveParallel(mig, a.parallelOverride)
	cursorKey := dataRollbackCursorPrefix + m.GetId()
	for i, op := range mig.Operations {
		done, err := a.opAlreadyComplete(ctx, cursorKey, i)
		if err != nil {
			return fmt.Errorf("redis rollbackYAMLDataMigration: cursor read op[%d]: %w", i, err)
		}
		if done {
			continue
		}
		if err := a.guardTransformResume(ctx, cursorKey, i, op); err != nil {
			return fmt.Errorf("redis rollbackYAMLDataMigration: %w", err)
		}
		if err := a.runDataOp(ctx, op, i, parallel, codec, vms); err != nil {
			return fmt.Errorf("redis rollbackYAMLDataMigration: op[%d] %s on %s: %w",
				i, op.Op, op.Keyspace, err)
		}
		if err := a.markOpComplete(ctx, cursorKey, i); err != nil {
			return fmt.Errorf("redis rollbackYAMLDataMigration: cursor write op[%d]: %w", i, err)
		}
	}
	if err := a.recordMigrationRolledBack(ctx, m); err != nil {
		return fmt.Errorf("redis rollbackYAMLDataMigration: bookkeeping: %w", err)
	}
	if err := a.clearCursor(ctx, cursorKey); err != nil {
		return fmt.Errorf("redis rollbackYAMLDataMigration: cursor clear: %w", err)
	}
	return nil
}

// runDataOp dispatches one Operation against the Redis client.
// Producer goroutine SCANs the keyspace and pushes keys onto a
// buffered channel; `parallel` worker goroutines read from the
// channel and run applyOpToKey on each key. errgroup propagates
// the first failure + cancels the rest.
//
// SCAN with COUNT=100 — small enough to be responsive, large
// enough to amortise the round-trip cost. The producer is
// inherently single-threaded (Redis SCAN holds a server-side
// cursor); fan-out happens after each batch so workers run
// concurrently with the next SCAN page.
func (a *Applier) runDataOp(ctx context.Context, op datamigrate.Operation, opIdx, parallel int, codec *datamigrate.ProtoCodec, vms map[int]*datamigrate.TransformVM) error {
	if parallel < 1 {
		parallel = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	keys := make(chan string, parallel*4)

	g.Go(func() error {
		defer close(keys)
		var cursor uint64
		for {
			batch, next, err := a.client.Scan(gctx, cursor, op.Keyspace, 100).Result()
			if err != nil {
				return fmt.Errorf("scan %s: %w", op.Keyspace, err)
			}
			for _, k := range batch {
				select {
				case keys <- k:
				case <-gctx.Done():
					return gctx.Err()
				}
			}
			if next == 0 {
				return nil
			}
			cursor = next
		}
	})

	for w := 0; w < parallel; w++ {
		g.Go(func() error {
			for key := range keys {
				if err := a.applyOpToKey(gctx, op, key, codec, vms, opIdx); err != nil {
					return fmt.Errorf("key %s: %w", key, err)
				}
			}
			return nil
		})
	}

	return g.Wait()
}

// applyOpToKey is the per-key read-modify-write loop.
// Dispatch order:
//
//   - TRANSFORM_FIELD with a compiled VM goes through the
//     Starlark codec (encoding-agnostic; user's script is the
//     transform).
//   - Otherwise, encoding=protobuf migrations go through the
//     pre-built ProtoCodec.
//   - Otherwise, JSON-encoded migrations go through the lib
//     JSONApplyOp helper.
//
// Empty values short-circuit at the lib helper level so all
// three paths behave consistently.
func (a *Applier) applyOpToKey(ctx context.Context, op datamigrate.Operation, key string, codec *datamigrate.ProtoCodec, vms map[int]*datamigrate.TransformVM, opIdx int) error {
	raw, err := a.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return nil // race: key disappeared between SCAN and GET
		}
		return fmt.Errorf("GET: %w", err)
	}

	var (
		newRaw  []byte
		changed bool
	)
	switch {
	case op.Op == datamigrate.OpTransformField && vms != nil && vms[opIdx] != nil:
		newRaw, changed, err = vms[opIdx].Apply(raw)
	case codec != nil:
		newRaw, changed, err = codec.ApplyOp(raw, op)
	default:
		newRaw, changed, err = datamigrate.JSONApplyOp(raw, op)
	}
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	// KeepTTL (KEEPTTL flag) preserves any expiry the key already
	// carried — a plain SET would silently make a TTL'd key
	// immortal. applyOpToKey only reaches here after a successful
	// GET, so the key existed; KeepTTL is a no-op for keys that
	// never had a TTL.
	if err := a.client.Set(ctx, key, newRaw, goredis.KeepTTL).Err(); err != nil {
		return fmt.Errorf("SET: %w", err)
	}
	return nil
}

// opAlreadyComplete reports whether op `index` has been
// recorded as complete on the cursor side-channel for the
// migration. HEXISTS returns 0/1; missing key = 0 = false.
func (a *Applier) opAlreadyComplete(ctx context.Context, cursorKey string, index int) (bool, error) {
	field := strconv.Itoa(index)
	n, err := a.client.HExists(ctx, cursorKey, field).Result()
	if err != nil {
		return false, err
	}
	return n, nil
}

// markOpComplete records op `index` as complete on the cursor
// side-channel. HSET is fire-and-forget — Redis returns "1 new
// field" or "0 already existed", both fine.
func (a *Applier) markOpComplete(ctx context.Context, cursorKey string, index int) error {
	field := strconv.Itoa(index)
	return a.client.HSet(ctx, cursorKey, field, "complete").Err()
}

// transformStartedField is the cursor-hash field marking that a
// TRANSFORM_FIELD op has begun mutating keys. Namespaced with a
// `started:` prefix so it never collides with the numeric
// op-complete field markOpComplete writes.
func transformStartedField(index int) string {
	return "started:" + strconv.Itoa(index)
}

// guardTransformResume protects the one op kind the op-granularity
// cursor can't make replay-safe: TRANSFORM_FIELD runs a user
// Starlark script that may be non-idempotent, so re-running a
// half-completed op double-applies it to keys it already mutated.
//
// No-op for every other op kind (ADD_FIELD_DEFAULT / REMOVE_FIELD /
// RENAME_FIELD are each idempotent per key). For TRANSFORM_FIELD it
// records a durable "started" marker BEFORE the op mutates anything;
// on resume an op found started-but-not-complete is refused with an
// operator-actionable error rather than silently re-transformed. An
// un-started op runs normally (provably no prior partial apply).
func (a *Applier) guardTransformResume(ctx context.Context, cursorKey string, index int, op datamigrate.Operation) error {
	if op.Op != datamigrate.OpTransformField {
		return nil
	}
	field := transformStartedField(index)
	started, err := a.client.HExists(ctx, cursorKey, field).Result()
	if err != nil {
		return fmt.Errorf("cursor start-marker read op[%d]: %w", index, err)
	}
	if started {
		return fmt.Errorf("op[%d] TRANSFORM_FIELD on %s was interrupted mid-op: a non-idempotent Starlark transform cannot be safely resumed at op granularity (it would re-apply to keys already mutated before the interruption). Verify/restore the keyspace, then `HDEL %s %s` to force a re-run", index, op.Keyspace, cursorKey, field)
	}
	if err := a.client.HSet(ctx, cursorKey, field, "started").Err(); err != nil {
		return fmt.Errorf("cursor start-marker write op[%d]: %w", index, err)
	}
	return nil
}

// clearCursor deletes the per-migration cursor hash. Called
// after the bookkeeping write completes — no point keeping the
// cursor around for a fully-recorded migration. Idempotent:
// DEL on a missing key returns 0 + nil err.
func (a *Applier) clearCursor(ctx context.Context, cursorKey string) error {
	return a.client.Del(ctx, cursorKey).Err()
}

// recordMigrationApplied writes one row to the wc:migrations
// bookkeeping hash. Mirrors what `applied.Redis().RecordVersion`
// does for non-YAML bodies, only it lives in the apply tool
// instead of being baked into the migration body.
func (a *Applier) recordMigrationApplied(ctx context.Context, m *applyfetchpb.Migration) error {
	hash := sha256.Sum256([]byte(m.GetUpSql()))
	return a.client.HSet(ctx, "wc:migrations", m.GetId(), hex.EncodeToString(hash[:])).Err()
}

// recordMigrationRolledBack erases the bookkeeping row.
// HDEL is idempotent — safe to call on already-removed entries.
func (a *Applier) recordMigrationRolledBack(ctx context.Context, m *applyfetchpb.Migration) error {
	return a.client.HDel(ctx, "wc:migrations", m.GetId()).Err()
}

// isIrreversibleMarkerBody returns true when the body is a
// pure `# wc:irreversible:` comment block — what emit/redis
// produces in the down direction when the forward direction
// contained REMOVE_FIELD. Distinguishes "rollback refused" from
// "rollback applies a YAML body".
func isIrreversibleMarkerBody(body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	lines := strings.Split(body, "\n")
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, "#") {
			return false
		}
		if strings.Contains(ln, "wc:irreversible") {
			return true
		}
	}
	return false
}
