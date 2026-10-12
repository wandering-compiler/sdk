package s3_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
)

// condFake is a single-object S3 endpoint that honours the
// conditional headers the run-lock relies on: PutObject
// If-None-Match:* (create-if-absent) and If-Match:<etag> (compare-
// and-swap), plus GetObject / DeleteObject(If-Match). It assigns a
// fresh ETag per successful write so takeover races resolve to one
// winner. Path is ignored — there's exactly one lock object.
//
// Like a real store it reports its OWN clock: Last-Modified is stamped
// from clk at each write and Date from clk at each answer, so a test
// controls server time independently of each client's clock.
type condFake struct {
	mu       sync.Mutex
	body     []byte
	etag     string
	exists   bool
	counter  int
	clk      *clock
	modified time.Time
}

func (f *condFake) stampDate(w http.ResponseWriter) {
	w.Header().Set("Date", f.clk.now().UTC().Format(http.TimeFormat))
}

func (f *condFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stampDate(w)
	switch r.Method {
	case http.MethodPut:
		inm := r.Header.Get("If-None-Match")
		ifm := r.Header.Get("If-Match")
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
		f.modified = f.clk.now()
		w.Header().Set("ETag", f.etag)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if !f.exists {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", f.etag)
		w.Header().Set("Last-Modified", f.modified.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.body)
	case http.MethodDelete:
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

// clock is a controllable wall clock for staleness assertions.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func lockServer(t *testing.T, serverClock *clock) string {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_required")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	srv := httptest.NewServer(&condFake{clk: serverClock})
	t.Cleanup(srv.Close)
	return srv.URL
}

func lockApplier(t *testing.T, endpoint string, now func() time.Time, stale, beat time.Duration) *s3.Applier {
	t.Helper()
	dsn := "s3://bucket?endpoint=" + url.QueryEscape(endpoint) + "&region=us-east-1"
	a, err := s3.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.SetLockTunablesForTest(now, stale, beat)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// Core invariant on S3: while one run holds the
// lock object, a second run is refused; releasing deletes it so the
// next run can acquire.
func TestS3RunLock_MutualExclusion(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	ep := lockServer(t, clk)
	ctx := context.Background()

	a1 := lockApplier(t, ep, clk.now, 10*time.Minute, time.Hour)
	a2 := lockApplier(t, ep, clk.now, 10*time.Minute, time.Hour)

	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := a2.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("contended acquire = %v, want ErrLockHeld", err)
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	l2, err := a2.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := l2.Release(ctx); err != nil {
		t.Fatalf("release l2: %v", err)
	}
}

// A crashed holder (no heartbeat) goes stale after staleAfter; the
// next run takes it over by compare-and-swap, and the zombie holder's
// late Release must NOT delete the new owner's lock (If-Match guard).
func TestS3RunLock_StaleTakeover_ZombieReleaseNoOp(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	ep := lockServer(t, clk)
	ctx := context.Background()

	a1 := lockApplier(t, ep, clk.now, 10*time.Minute, time.Hour) // beat=1h → never refreshes
	a2 := lockApplier(t, ep, clk.now, 10*time.Minute, time.Hour)

	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a1 acquire: %v", err)
	}
	// Not yet stale → contended.
	if _, err := a2.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("fresh-lock acquire = %v, want ErrLockHeld", err)
	}
	// a1 "crashes": advance past staleAfter with no heartbeat.
	clk.advance(11 * time.Minute)

	l2, err := a2.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("takeover acquire: %v", err)
	}
	// Zombie a1 releases late — must not delete a2's lock, and must SAY
	// the lock was lost: part of a1's run was not protected by it.
	if err := l1.Release(ctx); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("zombie release = %v, want ErrLockLost", err)
	}
	// a2 still holds → a fresh contender is still refused.
	a3 := lockApplier(t, ep, clk.now, 10*time.Minute, time.Hour)
	if _, err := a3.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("a3 acquire = %v, want ErrLockHeld (a2 holds)", err)
	}
	if err := l2.Release(ctx); err != nil {
		t.Fatalf("release l2: %v", err)
	}
}

// The heartbeat refresh re-stamps heartbeat_at so a long run isn't
// taken over. Drive one refresh after advancing past staleAfter and
// confirm a would-be taker is refused (the lock is fresh again).
func TestS3RunLock_RefreshPreventsTakeover(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	ep := lockServer(t, clk)
	ctx := context.Background()

	a1 := lockApplier(t, ep, clk.now, 10*time.Minute, time.Hour)
	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	clk.advance(11 * time.Minute) // would be stale now
	if err := s3.RefreshForTest(l1); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// heartbeat_at is now current → a taker at the same clock sees a
	// live lock and is refused (without the refresh it would take over).
	a2 := lockApplier(t, ep, clk.now, 10*time.Minute, time.Hour)
	if _, err := a2.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("acquire after refresh = %v, want ErrLockHeld", err)
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// Staleness is judged on the STORE's clock. A host whose own clock runs
// far ahead of the holder's must not see a live lock as abandoned: with
// the server clock unmoved, the lock was refreshed "just now" whatever the
// taker's wall clock says.
func TestS3RunLock_ClockSkewedTakerCannotTakeOverALiveLock(t *testing.T) {
	server := &clock{t: time.Unix(1_700_000_000, 0)}
	ep := lockServer(t, server)
	ctx := context.Background()

	holder := lockApplier(t, ep, server.now, 10*time.Minute, time.Hour)
	l1, err := holder.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	t.Cleanup(func() { _ = l1.Release(ctx) })

	ahead := &clock{t: server.now().Add(time.Hour)} // the taker's clock runs an hour fast
	taker := lockApplier(t, ep, ahead.now, 10*time.Minute, time.Hour)
	if _, err := taker.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("acquire from a host whose clock runs ahead = %v, want ErrLockHeld (the lock is live on the store's clock)", err)
	}
}

// A holder whose lock was taken over must stop before its next write, and
// say so — not go on writing next to the run that now holds the lock.
func TestS3RunLock_LostLockStopsTheNextWrite(t *testing.T) {
	server := &clock{t: time.Unix(1_700_000_000, 0)}
	ep := lockServer(t, server)
	ctx := context.Background()

	a1 := lockApplier(t, ep, server.now, 10*time.Minute, time.Hour)
	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a1 acquire: %v", err)
	}
	server.advance(11 * time.Minute) // a1 paused past the stale window
	a2 := lockApplier(t, ep, server.now, 10*time.Minute, time.Hour)
	l2, err := a2.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a2 takeover: %v", err)
	}
	t.Cleanup(func() { _ = l2.Release(ctx) })

	// a1 wakes; its next heartbeat finds the object replaced.
	if err := s3.RefreshForTest(l1); err == nil {
		t.Fatal("refresh of a lock another run took over must fail")
	}
	cl, ok := l1.(migrate.CheckedRunLock)
	if !ok {
		t.Fatal("the S3 run lock must be a CheckedRunLock")
	}
	if err := cl.Check(ctx); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check after takeover = %v, want ErrLockLost", err)
	}
	err = a1.Apply(ctx, &applyfetchpb.Migration{Id: "20260101T000000Z", UpSql: "S3 PUT data/x.json {}"})
	if !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Apply after the lock was lost = %v, want ErrLockLost before anything is written", err)
	}
}
