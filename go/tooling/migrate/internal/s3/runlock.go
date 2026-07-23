package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/runlock"
)

var _ migrate.RunLockCapable = (*Applier)(nil)

// runLockKey is the apply run-lock object. Kept OUTSIDE trackerPrefix
// (`wc-migrations/`) so AppliedHead's listing never mistakes the lock
// for a recorded migration.
const runLockKey = "wc-migrate-runlock.json"

// lockBody is the JSON persisted at runLockKey. heartbeat_at is the
// staleness clock — a holder refreshes it; a reader treats a lock
// whose heartbeat_at is older than staleAfter as abandoned.
type lockBody struct {
	Owner       string `json:"owner"`
	AcquiredAt  int64  `json:"acquired_at"`
	HeartbeatAt int64  `json:"heartbeat_at"`
}

func (a *Applier) now() time.Time {
	if a.nowFn != nil {
		return a.nowFn()
	}
	return time.Now()
}

func (a *Applier) staleAfter() time.Duration {
	if a.lockStale > 0 {
		return a.lockStale
	}
	return runlock.StaleAfter
}

// s3RunLock is a held lock. etag is the lock object's current ETag,
// required so refresh / release only ever touch the exact object this
// process wrote (an If-Match guard) — never one a taker re-created.
type s3RunLock struct {
	a     *Applier
	cli   *awss3.Client
	owner string

	acquiredAt int64 // preserved across heartbeats (informational)

	mu     sync.Mutex // guards etag against the heartbeat / Release race
	etag   string
	beater *runlock.Beater
}

// AcquireRunLock implements migrate.RunLockCapable for S3. S3 has no
// native TTL, so staleness is timestamp-based: acquisition is an
// atomic create-if-absent (PutObject If-None-Match:*), and a lock
// whose heartbeat_at has gone stale is taken over by atomically
// overwriting that exact object (PutObject If-Match:<etag>).
func (a *Applier) AcquireRunLock(ctx context.Context) (migrate.RunLock, error) {
	cli, err := a.s3Client(ctx)
	if err != nil {
		return nil, err
	}
	owner := runlock.OwnerID()
	etag, acquiredAt, err := a.tryAcquire(ctx, cli, owner)
	if err != nil {
		return nil, err
	}
	l := &s3RunLock{a: a, cli: cli, owner: owner, etag: etag, acquiredAt: acquiredAt}
	beat := a.lockBeat
	if beat <= 0 {
		beat = runlock.Heartbeat
	}
	// A swallowed heartbeat failure (e.g. a taker replaced us via
	// If-Match, or transient PutObject errors) lets the lock go stale,
	// after which a second run can take it over and double-apply a
	// non-idempotent migration. Surface it as a WARNING (the beater
	// has no return path).
	l.beater = runlock.StartBeaterInterval(ctx, beat, l.refreshOnce, func(err error) {
		slog.Warn("run-lock heartbeat refresh failed (lock may expire; concurrent run could double-apply)",
			slog.String("error", err.Error()))
	})
	return l, nil
}

// tryAcquire: create-if-absent, else take over only a stale lock,
// else migrate.ErrLockHeld. Returns the held object's ETag.
func (a *Applier) tryAcquire(ctx context.Context, cli *awss3.Client, owner string) (etag string, acquiredAt int64, err error) {
	now := a.now()
	acquiredAt = now.Unix()
	body, err := json.Marshal(lockBody{Owner: owner, AcquiredAt: acquiredAt, HeartbeatAt: acquiredAt})
	if err != nil {
		return "", 0, fmt.Errorf("s3 run-lock: marshal: %w", err)
	}

	// First-acquire: atomic create-if-absent.
	out, err := cli.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(a.bucket),
		Key:         aws.String(runLockKey),
		Body:        bytes.NewReader(body),
		IfNoneMatch: aws.String("*"),
	})
	if err == nil {
		return aws.ToString(out.ETag), acquiredAt, nil
	}
	if !isPreconditionFailed(err) {
		return "", 0, fmt.Errorf("s3 acquire run-lock: %w", err)
	}

	// Object exists — read it and take over only if it has gone stale.
	cur, curETag, gerr := a.readLock(ctx, cli)
	if gerr != nil {
		if isNoSuchKey(gerr) {
			// The holder released between our PUT and our GET. Stay
			// fail-fast simple: report held; a re-run acquires cleanly.
			return "", 0, migrate.ErrLockHeld
		}
		return "", 0, fmt.Errorf("s3 acquire run-lock: read existing: %w", gerr)
	}
	if now.Unix()-cur.HeartbeatAt <= int64(a.staleAfter().Seconds()) {
		return "", 0, migrate.ErrLockHeld // a live holder is refreshing it
	}

	// Stale: atomically replace THIS exact object. If-Match means only
	// one taker wins; a loser sees a changed ETag → ErrLockHeld.
	out, err = cli.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:  aws.String(a.bucket),
		Key:     aws.String(runLockKey),
		Body:    bytes.NewReader(body),
		IfMatch: aws.String(curETag),
	})
	if err != nil {
		if isPreconditionFailed(err) {
			return "", 0, migrate.ErrLockHeld
		}
		return "", 0, fmt.Errorf("s3 take over stale run-lock: %w", err)
	}
	return aws.ToString(out.ETag), acquiredAt, nil
}

func (a *Applier) readLock(ctx context.Context, cli *awss3.Client) (lockBody, string, error) {
	got, err := cli.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(runLockKey),
	})
	if err != nil {
		return lockBody{}, "", err
	}
	raw, rerr := io.ReadAll(got.Body)
	cerr := got.Body.Close()
	if rerr != nil {
		return lockBody{}, "", rerr
	}
	if cerr != nil {
		return lockBody{}, "", cerr
	}
	var lb lockBody
	if err := json.Unmarshal(raw, &lb); err != nil {
		return lockBody{}, "", fmt.Errorf("decode lock: %w", err)
	}
	return lb, aws.ToString(got.ETag), nil
}

// refreshOnce re-stamps heartbeat_at, guarded by If-Match so it only
// writes if we still hold the exact object. Losing the If-Match means
// a taker replaced us — surfaced as an error to the beater's onErr.
func (l *s3RunLock) refreshOnce(ctx context.Context) error {
	l.mu.Lock()
	etag := l.etag
	l.mu.Unlock()

	now := l.a.now()
	body, err := json.Marshal(lockBody{Owner: l.owner, AcquiredAt: l.acquiredAt, HeartbeatAt: now.Unix()})
	if err != nil {
		return err
	}
	out, err := l.cli.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:  aws.String(l.a.bucket),
		Key:     aws.String(runLockKey),
		Body:    bytes.NewReader(body),
		IfMatch: aws.String(etag),
	})
	if err != nil {
		return fmt.Errorf("s3 refresh run-lock: %w", err)
	}
	l.mu.Lock()
	l.etag = aws.ToString(out.ETag)
	l.mu.Unlock()
	return nil
}

// Release stops the heartbeat then deletes the lock object iff we
// still own it (If-Match our ETag). A precondition failure / missing
// object means a taker already owns it — nothing of ours to free.
func (l *s3RunLock) Release(ctx context.Context) error {
	if l.beater != nil {
		l.beater.Stop()
		l.beater = nil
	}
	l.mu.Lock()
	etag := l.etag
	l.mu.Unlock()

	_, err := l.cli.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket:  aws.String(l.a.bucket),
		Key:     aws.String(runLockKey),
		IfMatch: aws.String(etag),
	})
	if err != nil {
		if isPreconditionFailed(err) || isNoSuchKey(err) {
			return nil
		}
		return fmt.Errorf("s3 release run-lock: %w", err)
	}
	return nil
}

// isPreconditionFailed reports whether err is an S3 412 — a failed
// If-None-Match (object already exists) or If-Match (etag moved).
func isPreconditionFailed(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "PreconditionFailed" {
		return true
	}
	var statusErr interface{ HTTPStatusCode() int }
	if errors.As(err, &statusErr) && statusErr.HTTPStatusCode() == 412 {
		return true
	}
	return false
}
