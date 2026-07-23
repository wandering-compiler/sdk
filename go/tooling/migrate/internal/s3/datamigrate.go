package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/errgroup"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/dmbuild"
)

// buildCodec / buildTransformVMs delegate to the shared dmbuild helpers; the
// build logic is backend-agnostic and lives in one place (see dmbuild).
func buildCodec(mig *datamigrate.Migration) (*datamigrate.ProtoCodec, error) {
	return dmbuild.Codec(mig)
}

func buildTransformVMs(mig *datamigrate.Migration) (map[int]*datamigrate.TransformVM, error) {
	return dmbuild.TransformVMs(mig)
}

// dataCursorPrefix is the well-known object-prefix for the
// per-migration op-completion side-channel (Phase E v2.1
// resumability — D-iter3-15 §Resumability + D-iter3-17 follow-
// up). One JSON object per migration id holding the list of
// completed op-indexes; consulted on each op start (skip if
// already completed) and DELETE'd after the bookkeeping write
// succeeds — a successful end-to-end run leaves no cursor
// object behind.
const dataCursorPrefix = "wc-data-migrations/"

// applyYAMLDataMigration handles migration bodies whose
// `up_sql` is a YAML data migration (Phase E — D-iter3-15).
// Mirrors apply/redis's path; the only difference is the I/O
// (ListObjectsV2 + GetObject + PutObject instead of SCAN +
// GET + SET).
//
// Per-object updates are replay-safe for the SCHEMA op kinds:
//   - ADD_FIELD_DEFAULT only sets when missing
//   - REMOVE_FIELD is naturally idempotent
//   - RENAME_FIELD only renames when From exists
//
// TRANSFORM_FIELD is the exception: its user Starlark script may
// be non-idempotent, so an op-granularity resume would re-apply
// it to objects it already mutated before the interruption. We
// mark each TRANSFORM_FIELD op "started" before it mutates
// anything and REFUSE to silently resume one found started-but-
// not-complete (see guardTransformResume).
//
// v2.1 adds parallel workers (errgroup pool of N goroutines)
// driven by the migration's `parallel:` field or operator
// `--parallel` override + an op-granularity cursor that lets
// interrupted apply runs resume by skipping already-completed
// ops on retry.
//
// Bookkeeping write at the end (`PutObject
// wc-migrations/<id>.json` carrying the integrity envelope
// applied.S3() emits) — applied.Wrap is intentionally skipped
// at registry-side for YAML bodies.
func (a *Applier) applyYAMLDataMigration(ctx context.Context, m *applyfetchpb.Migration) error {
	mig, err := datamigrate.Unmarshal([]byte(m.GetUpSql()))
	if err != nil {
		return fmt.Errorf("s3 applyYAMLDataMigration: parse: %w", err)
	}
	codec, err := buildCodec(mig)
	if err != nil {
		return fmt.Errorf("s3 applyYAMLDataMigration: %w", err)
	}
	vms, err := buildTransformVMs(mig)
	if err != nil {
		return fmt.Errorf("s3 applyYAMLDataMigration: %w", err)
	}
	cli, err := a.s3Client(ctx)
	if err != nil {
		return err
	}
	parallel := datamigrate.EffectiveParallel(mig, a.parallelOverride)
	cursorKey := dataCursorPrefix + m.GetId() + ".cursor.json"
	cursor, err := a.loadCursor(ctx, cli, cursorKey)
	if err != nil {
		return fmt.Errorf("s3 applyYAMLDataMigration: cursor read: %w", err)
	}
	for i, op := range mig.Operations {
		if cursor.has(i) {
			continue
		}
		if err := a.guardTransformResume(ctx, cli, cursorKey, cursor, i, op); err != nil {
			return fmt.Errorf("s3 applyYAMLDataMigration: %w", err)
		}
		if err := a.runDataOp(ctx, cli, op, i, parallel, codec, vms); err != nil {
			return fmt.Errorf("s3 applyYAMLDataMigration: op[%d] %s on %s: %w",
				i, op.Op, op.Keyspace, err)
		}
		cursor.add(i)
		if err := a.saveCursor(ctx, cli, cursorKey, cursor); err != nil {
			return fmt.Errorf("s3 applyYAMLDataMigration: cursor write op[%d]: %w", i, err)
		}
	}
	if err := a.recordMigrationApplied(ctx, cli, m); err != nil {
		return fmt.Errorf("s3 applyYAMLDataMigration: bookkeeping: %w", err)
	}
	if err := a.deleteCursor(ctx, cli, cursorKey); err != nil {
		return fmt.Errorf("s3 applyYAMLDataMigration: cursor delete: %w", err)
	}
	return nil
}

// rollbackYAMLDataMigration is the inverse path. Down body
// either (a) auto-derived YAML inverse from emit/s3 or
// (b) `# wc:irreversible:` comment block. Latter refuses
// without --allow-irreversible.
//
// Cursor key uses `.rollback.cursor.json` suffix so a partial
// rollback resumes against its own state independent of any
// forward cursor still present.
func (a *Applier) rollbackYAMLDataMigration(ctx context.Context, m *applyfetchpb.Migration) error {
	body := m.GetDownSql()
	if isIrreversibleMarkerBody(body) {
		return errors.New("s3 rollbackYAMLDataMigration: down body is irreversible (REMOVE_FIELD has no inverse) — refused without --allow-irreversible")
	}
	mig, err := datamigrate.Unmarshal([]byte(body))
	if err != nil {
		return fmt.Errorf("s3 rollbackYAMLDataMigration: parse: %w", err)
	}
	codec, err := buildCodec(mig)
	if err != nil {
		return fmt.Errorf("s3 rollbackYAMLDataMigration: %w", err)
	}
	vms, err := buildTransformVMs(mig)
	if err != nil {
		return fmt.Errorf("s3 rollbackYAMLDataMigration: %w", err)
	}
	cli, err := a.s3Client(ctx)
	if err != nil {
		return err
	}
	parallel := datamigrate.EffectiveParallel(mig, a.parallelOverride)
	cursorKey := dataCursorPrefix + m.GetId() + ".rollback.cursor.json"
	cursor, err := a.loadCursor(ctx, cli, cursorKey)
	if err != nil {
		return fmt.Errorf("s3 rollbackYAMLDataMigration: cursor read: %w", err)
	}
	for i, op := range mig.Operations {
		if cursor.has(i) {
			continue
		}
		if err := a.guardTransformResume(ctx, cli, cursorKey, cursor, i, op); err != nil {
			return fmt.Errorf("s3 rollbackYAMLDataMigration: %w", err)
		}
		if err := a.runDataOp(ctx, cli, op, i, parallel, codec, vms); err != nil {
			return fmt.Errorf("s3 rollbackYAMLDataMigration: op[%d] %s on %s: %w",
				i, op.Op, op.Keyspace, err)
		}
		cursor.add(i)
		if err := a.saveCursor(ctx, cli, cursorKey, cursor); err != nil {
			return fmt.Errorf("s3 rollbackYAMLDataMigration: cursor write op[%d]: %w", i, err)
		}
	}
	if err := a.recordMigrationRolledBack(ctx, cli, m); err != nil {
		return fmt.Errorf("s3 rollbackYAMLDataMigration: bookkeeping: %w", err)
	}
	if err := a.deleteCursor(ctx, cli, cursorKey); err != nil {
		return fmt.Errorf("s3 rollbackYAMLDataMigration: cursor delete: %w", err)
	}
	return nil
}

// runDataOp dispatches one Operation against the S3 client.
// Producer goroutine pages ListObjectsV2 under the keyspace
// prefix and pushes object keys onto a buffered channel;
// `parallel` workers consume from the channel and run
// applyOpToObject on each. errgroup propagates the first error
// + cancels remaining workers via the shared context.
func (a *Applier) runDataOp(ctx context.Context, cli *awss3.Client, op datamigrate.Operation, opIdx, parallel int, codec *datamigrate.ProtoCodec, vms map[int]*datamigrate.TransformVM) error {
	if parallel < 1 {
		parallel = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	keys := make(chan string, parallel*4)

	g.Go(func() error {
		defer close(keys)
		var token *string
		for {
			out, err := cli.ListObjectsV2(gctx, &awss3.ListObjectsV2Input{
				Bucket:            aws.String(a.bucket),
				Prefix:            aws.String(op.Keyspace),
				ContinuationToken: token,
			})
			if err != nil {
				return fmt.Errorf("ListObjectsV2 %s: %w", op.Keyspace, err)
			}
			for _, obj := range out.Contents {
				key := aws.ToString(obj.Key)
				select {
				case keys <- key:
				case <-gctx.Done():
					return gctx.Err()
				}
			}
			if out.IsTruncated == nil || !*out.IsTruncated {
				return nil
			}
			token = out.NextContinuationToken
		}
	})

	for w := 0; w < parallel; w++ {
		g.Go(func() error {
			for key := range keys {
				if err := a.applyOpToObject(gctx, cli, op, key, codec, vms, opIdx); err != nil {
					return fmt.Errorf("object %s: %w", key, err)
				}
			}
			return nil
		})
	}

	return g.Wait()
}

// applyOpToObject is the per-object read-modify-write loop.
// Dispatch order matches apply/redis (TRANSFORM_FIELD via
// Starlark VM > protobuf codec > JSON helper). Empty bodies
// short-circuit at the lib helper.
func (a *Applier) applyOpToObject(ctx context.Context, cli *awss3.Client, op datamigrate.Operation, key string, codec *datamigrate.ProtoCodec, vms map[int]*datamigrate.TransformVM, opIdx int) error {
	got, err := cli.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return nil // race: object disappeared between List + Get
		}
		return fmt.Errorf("GetObject: %w", err)
	}
	raw, err := io.ReadAll(got.Body)
	closeErr := got.Body.Close()
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close body: %w", closeErr)
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

	// Carry the original object's metadata onto the rewrite. A
	// PutObject with only Bucket/Key/Body would reset the object to
	// S3's defaults — stripping the Content-Type, user Metadata, and
	// the content-* headers the original upload set. Read them off
	// the GetObjectOutput (plain struct fields, valid after the body
	// is consumed) and replay them verbatim.
	if _, err := cli.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:             aws.String(a.bucket),
		Key:                aws.String(key),
		Body:               bytes.NewReader(newRaw),
		ContentType:        got.ContentType,
		Metadata:           got.Metadata,
		CacheControl:       got.CacheControl,
		ContentEncoding:    got.ContentEncoding,
		ContentDisposition: got.ContentDisposition,
		ContentLanguage:    got.ContentLanguage,
	}); err != nil {
		return fmt.Errorf("PutObject: %w", err)
	}
	return nil
}

// dataCursor is the JSON shape persisted at dataCursorPrefix +
// "<id>.cursor.json". Records which op-indexes have been fully
// processed for the migration so a re-run skips them. StartedOps
// additionally records TRANSFORM_FIELD ops that began mutating
// objects but didn't complete — a non-idempotent transform found
// started-but-not-complete is refused rather than re-applied
// (see guardTransformResume). `omitempty` keeps pre-existing
// cursor files (no started_ops key) round-tripping unchanged.
type dataCursor struct {
	CompletedOps []int `json:"completed_ops"`
	StartedOps   []int `json:"started_ops,omitempty"`
}

func (c *dataCursor) has(index int) bool {
	for _, i := range c.CompletedOps {
		if i == index {
			return true
		}
	}
	return false
}

func (c *dataCursor) add(index int) {
	if c.has(index) {
		return
	}
	c.CompletedOps = append(c.CompletedOps, index)
}

func (c *dataCursor) hasStarted(index int) bool {
	for _, i := range c.StartedOps {
		if i == index {
			return true
		}
	}
	return false
}

func (c *dataCursor) addStarted(index int) {
	if c.hasStarted(index) {
		return
	}
	c.StartedOps = append(c.StartedOps, index)
}

// guardTransformResume protects TRANSFORM_FIELD — the one op kind
// the op-granularity cursor can't make replay-safe, because its
// user Starlark script may be non-idempotent (e.g. `x = x * 2`).
// No-op for every other op kind. For TRANSFORM_FIELD it records a
// durable "started" marker (persisted via saveCursor) BEFORE the
// op mutates anything; on resume an op found started-but-not-
// complete is refused with an operator-actionable error rather
// than silently re-applied. An un-started op runs normally.
func (a *Applier) guardTransformResume(ctx context.Context, cli *awss3.Client, cursorKey string, cursor *dataCursor, index int, op datamigrate.Operation) error {
	if op.Op != datamigrate.OpTransformField {
		return nil
	}
	if cursor.hasStarted(index) {
		return fmt.Errorf("op[%d] TRANSFORM_FIELD on %s was interrupted mid-op: a non-idempotent Starlark transform cannot be safely resumed at op granularity (it would re-apply to objects already mutated before the interruption). Verify/restore the keyspace, then delete cursor object %q to force a re-run", index, op.Keyspace, cursorKey)
	}
	cursor.addStarted(index)
	if err := a.saveCursor(ctx, cli, cursorKey, cursor); err != nil {
		return fmt.Errorf("cursor start-marker write op[%d]: %w", index, err)
	}
	return nil
}

// loadCursor fetches the per-migration cursor object. Missing
// object = empty cursor (first run / no prior interrupt). Any
// other error surfaces.
func (a *Applier) loadCursor(ctx context.Context, cli *awss3.Client, key string) (*dataCursor, error) {
	got, err := cli.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return &dataCursor{}, nil
		}
		return nil, err
	}
	raw, err := io.ReadAll(got.Body)
	closeErr := got.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read cursor body: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close cursor body: %w", closeErr)
	}
	var c dataCursor
	if len(raw) == 0 {
		return &c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode cursor JSON: %w", err)
	}
	return &c, nil
}

// saveCursor PUTs the cursor JSON. Called after each op's
// completion so an interrupted run resumes from the next op.
func (a *Applier) saveCursor(ctx context.Context, cli *awss3.Client, key string, c *dataCursor) error {
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode cursor JSON: %w", err)
	}
	_, err = cli.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	return err
}

// deleteCursor removes the per-migration cursor object after
// the bookkeeping write succeeds. NoSuchKey is tolerated
// (idempotent re-run path: cursor was already cleaned up).
func (a *Applier) deleteCursor(ctx context.Context, cli *awss3.Client, key string) error {
	_, err := cli.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isNoSuchKey(err) {
		return err
	}
	return nil
}

// recordMigrationApplied writes the wc-migrations/<id>.json
// bookkeeping object. Mirrors what `applied.S3().RecordVersion`
// produces for non-YAML bodies — same shape, written here
// directly via PutObject because YAML bodies skip
// applied.Wrap on the registry side.
func (a *Applier) recordMigrationApplied(ctx context.Context, cli *awss3.Client, m *applyfetchpb.Migration) error {
	hash := sha256.Sum256([]byte(m.GetUpSql()))
	envelope := fmt.Sprintf(`{"timestamp":"%s","content_sha256":"%s"}`,
		m.GetId(), hex.EncodeToString(hash[:]))
	key := trackerPrefix + m.GetId() + ".json"
	_, err := cli.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte(envelope)),
	})
	return err
}

// recordMigrationRolledBack erases the bookkeeping object.
// NoSuchKey is tolerated (already removed = idempotent
// rollback).
func (a *Applier) recordMigrationRolledBack(ctx context.Context, cli *awss3.Client, m *applyfetchpb.Migration) error {
	key := trackerPrefix + m.GetId() + ".json"
	_, err := cli.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isNoSuchKey(err) {
		return err
	}
	return nil
}

// isIrreversibleMarkerBody — same shape as apply/redis's
// helper; pure `# wc:irreversible:` comment block produced by
// emit/s3 when REMOVE_FIELD is in the forward direction.
func isIrreversibleMarkerBody(body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	for _, ln := range strings.Split(body, "\n") {
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
