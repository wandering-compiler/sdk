// Package memory is the in-memory [kvfs.Driver]
// implementation used by unit tests. Production code never
// reaches for this — the local_fs driver is the production
// path for LOCAL_FS connections; future networked drivers
// (S3, MinIO) ship in their own subpackages.
//
// The driver is goroutine-safe: every Driver operation
// guards the underlying map with an RWMutex.
//
// PutFromTempFile reads the tmp file into memory, removes
// the tmp file (matching the local_fs contract), and
// inserts the bytes under the given key. The "atomic" part
// is satisfied by the lock — concurrent readers either see
// the prior bytes or the new ones, never a partial write.
package memory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// Driver is the in-memory store. Zero value usable; safe
// for concurrent use.
type Driver struct {
	mu      sync.RWMutex
	objects map[string]entry
	now     func() time.Time
}

type entry struct {
	body    []byte
	modTime time.Time
}

// New returns an empty in-memory driver.
func New() *Driver {
	return &Driver{objects: map[string]entry{}, now: time.Now}
}

// SetClock overrides the modtime source. Test-only — lets a
// fixture pin Last-Modified output deterministically.
func (d *Driver) SetClock(now func() time.Time) {
	d.mu.Lock()
	d.now = now
	d.mu.Unlock()
}

// PutFromTempFile reads tmpPath into memory, deletes the
// tmp file, and stores the bytes under key. Returns
// `handle = key`.
func (d *Driver) PutFromTempFile(_ context.Context, key string, tmpPath string) (string, error) {
	body, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}
	d.put(key, body)
	// Mirror the local_fs driver: tmp file is consumed by a
	// successful Put. Errors removing it after a successful
	// store are harmless (operator-side cleanup will catch
	// any leftover) so we swallow.
	_ = os.Remove(tmpPath)
	return key, nil
}

// PutBytes is a convenience for tests that want to seed the
// driver without going through a real tmp file. Not part of
// the [kvfs.Driver] interface.
func (d *Driver) PutBytes(key string, body []byte) {
	cp := make([]byte, len(body))
	copy(cp, body)
	d.put(key, cp)
}

func (d *Driver) put(key string, body []byte) {
	d.mu.Lock()
	if d.objects == nil {
		d.objects = map[string]entry{}
	}
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	d.objects[key] = entry{body: body, modTime: now()}
	d.mu.Unlock()
}

// Open returns a reader over the stored object's bytes.
// Returns [kvfs.ErrNotFound] when the key isn't present.
func (d *Driver) Open(_ context.Context, key string) (io.ReadCloser, error) {
	d.mu.RLock()
	e, ok := d.objects[key]
	d.mu.RUnlock()
	if !ok {
		return nil, kvfs.ErrNotFound
	}
	cp := make([]byte, len(e.body))
	copy(cp, e.body)
	return io.NopCloser(bytes.NewReader(cp)), nil
}

// Stat returns size + modtime for the stored object.
// Returns [kvfs.ErrNotFound] when the key isn't present.
func (d *Driver) Stat(_ context.Context, key string) (kvfs.Info, error) {
	d.mu.RLock()
	e, ok := d.objects[key]
	d.mu.RUnlock()
	if !ok {
		return kvfs.Info{}, kvfs.ErrNotFound
	}
	return kvfs.Info{Size: int64(len(e.body)), ModTime: e.modTime}, nil
}

// OpenSeekable returns a seek-capable reader over the
// stored object. Returns [kvfs.ErrNotFound] when missing.
func (d *Driver) OpenSeekable(_ context.Context, key string) (io.ReadSeekCloser, error) {
	d.mu.RLock()
	e, ok := d.objects[key]
	d.mu.RUnlock()
	if !ok {
		return nil, kvfs.ErrNotFound
	}
	cp := make([]byte, len(e.body))
	copy(cp, e.body)
	return readSeekNopCloser{Reader: bytes.NewReader(cp)}, nil
}

// readSeekNopCloser adapts *bytes.Reader (ReadSeeker) to
// ReadSeekCloser by adding a no-op Close.
type readSeekNopCloser struct{ *bytes.Reader }

func (readSeekNopCloser) Close() error { return nil }

// Delete removes the key. No error on missing keys.
func (d *Driver) Delete(_ context.Context, key string) error {
	d.mu.Lock()
	delete(d.objects, key)
	d.mu.Unlock()
	return nil
}

// Has reports whether a key is present. Test-only helper.
func (d *Driver) Has(key string) bool {
	d.mu.RLock()
	_, ok := d.objects[key]
	d.mu.RUnlock()
	return ok
}

// Keys returns every stored key in insertion-undefined
// order. Test-only helper for distribution checks.
func (d *Driver) Keys() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.objects))
	for k := range d.objects {
		out = append(out, k)
	}
	return out
}

// Compile-time guarantee that Driver implements kvfs.Driver.
var _ kvfs.Driver = (*Driver)(nil)

// Suppress unused-import lint on errors when the Open path
// stops returning the wrapped sentinel. Defensive.
var _ = errors.Is
