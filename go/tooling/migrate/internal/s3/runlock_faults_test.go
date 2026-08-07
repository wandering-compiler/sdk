package s3_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
)

// faultFake is a single-object conditional S3 endpoint (like condFake)
// with per-verb fault injection so the run-lock's error / race arms can
// be driven deterministically. A fault field >0 makes that verb return
// the given HTTP status BEFORE the normal conditional logic runs;
// failCreatePut targets the If-None-Match:* create, failMatchPut the
// If-Match compare-and-swap (so a takeover PUT can fail while the
// preceding create PUT 412s normally).
type faultFake struct {
	mu      sync.Mutex
	body    []byte
	etag    string
	exists  bool
	counter int

	failCreatePut int // status for PUT If-None-Match:*
	failMatchPut  int // status for PUT If-Match:<etag>
	failGet       int // status for GET
	failDelete    int // status for DELETE
}

func faultCode(status int) string {
	switch status {
	case http.StatusPreconditionFailed:
		return "PreconditionFailed"
	case http.StatusNotFound:
		return "NoSuchKey"
	default:
		return "InternalError"
	}
}

func (f *faultFake) set(fn func(*faultFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *faultFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		inm := r.Header.Get("If-None-Match")
		ifm := r.Header.Get("If-Match")
		if inm == "*" && f.failCreatePut > 0 {
			writeS3Error(w, f.failCreatePut, faultCode(f.failCreatePut))
			return
		}
		if ifm != "" && f.failMatchPut > 0 {
			writeS3Error(w, f.failMatchPut, faultCode(f.failMatchPut))
			return
		}
		if inm == "*" && f.exists {
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if ifm != "" && (!f.exists || ifm != f.etag) {
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.body = body
		f.exists = true
		f.counter++
		f.etag = fmt.Sprintf("%q", fmt.Sprintf("v%d", f.counter))
		w.Header().Set("ETag", f.etag)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if f.failGet > 0 {
			writeS3Error(w, f.failGet, faultCode(f.failGet))
			return
		}
		if !f.exists {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", f.etag)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.body)
	case http.MethodDelete:
		if f.failDelete > 0 {
			writeS3Error(w, f.failDelete, faultCode(f.failDelete))
			return
		}
		ifm := r.Header.Get("If-Match")
		if ifm != "" && (!f.exists || ifm != f.etag) {
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		f.exists = false
		f.body, f.etag = nil, ""
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func faultApplier(t *testing.T, f http.Handler, now func() time.Time, stale, beat time.Duration) *s3.Applier {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_required")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dsn := "s3://bucket?endpoint=" + url.QueryEscape(srv.URL) + "&region=us-east-1"
	a, err := s3.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.SetLockTunablesForTest(now, stale, beat)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func fixedClock() func() time.Time {
	t := time.Unix(1_700_000_000, 0)
	return func() time.Time { return t }
}

// staleLockBody is a valid lock object whose heartbeat is long past, so
// any realistic clock sees it as abandoned (drives the takeover arm).
const staleLockBody = `{"owner":"old","acquired_at":100,"heartbeat_at":100}`

// TestS3RunLock_FirstPutServerError pins the create-PUT non-precondition
// arm: a 500 on the initial If-None-Match:* create surfaces as a wrapped
// acquire error (not ErrLockHeld — that is reserved for a live holder).
func TestS3RunLock_FirstPutServerError(t *testing.T) {
	f := &faultFake{failCreatePut: http.StatusInternalServerError}
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, time.Hour)
	_, err := a.AcquireRunLock(context.Background())
	if err == nil || errors.Is(err, migrate.ErrLockHeld) ||
		!strings.Contains(err.Error(), "acquire run-lock") {
		t.Fatalf("AcquireRunLock = %v, want wrapped acquire error", err)
	}
}

// TestS3RunLock_ReadExistingError pins the readLock-failed arm: the
// create PUT 412s (lock present) but the follow-up GET errors with a
// non-NoSuchKey status, so acquisition reports the read failure rather
// than guessing the lock state.
func TestS3RunLock_ReadExistingError(t *testing.T) {
	f := &faultFake{failCreatePut: http.StatusPreconditionFailed, failGet: http.StatusInternalServerError}
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, time.Hour)
	_, err := a.AcquireRunLock(context.Background())
	if err == nil || errors.Is(err, migrate.ErrLockHeld) ||
		!strings.Contains(err.Error(), "read existing") {
		t.Fatalf("AcquireRunLock = %v, want read-existing error", err)
	}
}

// TestS3RunLock_HolderVanishedRace pins the create-then-GET race arm:
// the create PUT 412s (something exists) yet the GET 404s (the holder
// released in between). Rather than loop, acquisition reports ErrLockHeld
// so a clean re-run takes the now-free lock.
func TestS3RunLock_HolderVanishedRace(t *testing.T) {
	f := &faultFake{failCreatePut: http.StatusPreconditionFailed} // exists=false → GET 404
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, time.Hour)
	_, err := a.AcquireRunLock(context.Background())
	if !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("AcquireRunLock = %v, want ErrLockHeld", err)
	}
}

// TestS3RunLock_CorruptLockBody pins the decode arm of readLock: a
// present-but-unparseable lock object surfaces as a read-existing /
// decode error instead of a panic or a silent takeover.
func TestS3RunLock_CorruptLockBody(t *testing.T) {
	f := &faultFake{exists: true, etag: `"seed"`, body: []byte(`{not json`)}
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, time.Hour)
	_, err := a.AcquireRunLock(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read existing") {
		t.Fatalf("AcquireRunLock = %v, want decode error", err)
	}
}

// TestS3RunLock_TakeoverCASLoser pins the stale-takeover CAS-loser arm:
// the lock is stale (takeover proceeds) but the If-Match swap 412s — a
// concurrent taker won the race — so this caller backs off with
// ErrLockHeld rather than clobbering the winner.
func TestS3RunLock_TakeoverCASLoser(t *testing.T) {
	f := &faultFake{exists: true, etag: `"seed"`, body: []byte(staleLockBody), failMatchPut: http.StatusPreconditionFailed}
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, time.Hour)
	_, err := a.AcquireRunLock(context.Background())
	if !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("AcquireRunLock = %v, want ErrLockHeld", err)
	}
}

// TestS3RunLock_TakeoverServerError pins the stale-takeover non-precondition
// arm: a 500 on the If-Match swap surfaces as a wrapped takeover error.
func TestS3RunLock_TakeoverServerError(t *testing.T) {
	f := &faultFake{exists: true, etag: `"seed"`, body: []byte(staleLockBody), failMatchPut: http.StatusInternalServerError}
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, time.Hour)
	_, err := a.AcquireRunLock(context.Background())
	if err == nil || errors.Is(err, migrate.ErrLockHeld) ||
		!strings.Contains(err.Error(), "take over stale") {
		t.Fatalf("AcquireRunLock = %v, want takeover error", err)
	}
}

// TestS3RunLock_RefreshLostLock pins refreshOnce's error arm: after a
// clean acquire, an If-Match PUT that fails (a taker replaced us)
// surfaces as a refresh error to the beater's onErr.
func TestS3RunLock_RefreshLostLock(t *testing.T) {
	f := &faultFake{}
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, time.Hour)
	l, err := a.AcquireRunLock(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	f.set(func(ff *faultFake) { ff.failMatchPut = http.StatusInternalServerError })
	if err := s3.RefreshForTest(l); err == nil || !strings.Contains(err.Error(), "refresh run-lock") {
		t.Fatalf("RefreshForTest = %v, want refresh error", err)
	}
	f.set(func(ff *faultFake) { ff.failMatchPut = 0 })
	_ = l.Release(context.Background())
}

// TestS3RunLock_ReleaseServerError pins Release's non-precondition arm:
// a DeleteObject failure that is neither 412 nor NoSuchKey surfaces as a
// wrapped release error (a precondition / missing object stays a no-op).
func TestS3RunLock_ReleaseServerError(t *testing.T) {
	f := &faultFake{}
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, time.Hour)
	l, err := a.AcquireRunLock(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	f.set(func(ff *faultFake) { ff.failDelete = http.StatusInternalServerError })
	if err := l.Release(context.Background()); err == nil || !strings.Contains(err.Error(), "release run-lock") {
		t.Fatalf("Release = %v, want release error", err)
	}
}

// TestS3RunLock_DefaultBeatInterval pins the AcquireRunLock fallback
// where lockBeat is zero: the beater starts on the runlock default
// interval rather than panicking on a non-positive tick. A clean
// acquire + release with beat=0 exercises that arm.
func TestS3RunLock_DefaultBeatInterval(t *testing.T) {
	f := &faultFake{}
	a := faultApplier(t, f, fixedClock(), 10*time.Minute, 0) // beat=0 → runlock.Heartbeat
	l, err := a.AcquireRunLock(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := l.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
}
