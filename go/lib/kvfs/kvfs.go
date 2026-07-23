// Package kvfs is the key-value file store the
// gateway-level upload handler talks to. Implementations
// back UPLOADED_FILE / UPLOADED_IMAGE field types declared
// in the REST registry's multipart endpoints.
//
// Slice A (this file + the impl subpackages) ships the
// driver interface plus a hash-based sub-bucket key
// builder. Slice B wires the multipart streaming handler on
// top.
//
// Why a driver interface instead of using `os` directly:
//
//   - testability — the in-memory driver in `kvfs/memory`
//     keeps unit tests off the real disk (a thousand
//     uploaded fixtures during one `go test` run would
//     punish every developer's SSD).
//   - dialect symmetry — the same interface fits future
//     networked stores (S3, MinIO, GCS) so the gateway
//     emit path doesn't branch on dialect at runtime.
//   - atomic-put semantics — every implementation MUST
//     present a put-from-tmpfile primitive that's atomic
//     against concurrent readers (no half-written objects
//     visible mid-write). Local FS uses `os.Rename` within
//     one filesystem; S3 uses a single PutObject; memory
//     swaps a map entry under a write lock.
package kvfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// Driver is the storage primitive every kvfs backend
// implements. Methods take a context so cancellation /
// timeouts propagate from the gateway request down to the
// underlying I/O.
//
// Keys are caller-managed strings. Convention is
// `<bucket_path>/<sub_bucket_path>/<object_key>` produced
// by [BuildKey], but the driver itself doesn't validate the
// shape — it stores whatever string the caller passes.
type Driver interface {
	// PutFromTempFile atomically moves the file at tmpPath
	// into storage at key. The implementation MUST guarantee
	// that concurrent readers either see the previous value
	// (or "missing" if first write) or the new value, never
	// a half-written body. After a successful Put the tmp
	// file is gone (renamed or copied + unlinked); on error
	// the tmp file is left in place so the caller can
	// inspect / clean up.
	//
	// The returned `handle` is normally equal to `key`; it
	// exists as a separate return value so future drivers
	// that rewrite keys (e.g. content-addressed storage that
	// derives the final key from a hash) can surface the
	// canonical handle to the caller.
	PutFromTempFile(ctx context.Context, key string, tmpPath string) (handle string, err error)

	// Open returns a streaming reader for the stored object.
	// Returns ErrNotFound for a missing key.
	Open(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the stored object. Idempotent on
	// missing keys (no error when the key was already gone).
	Delete(ctx context.Context, key string) error

	// Stat reports the object's size and modification time.
	// Returns ErrNotFound for a missing key. Used by the
	// download gateway handler to populate Content-Length and
	// Last-Modified before streaming the body.
	Stat(ctx context.Context, key string) (Info, error)

	// OpenSeekable returns a seek-capable reader for the
	// stored object. Required by http.ServeContent to honour
	// Range requests without buffering the whole body.
	// Returns ErrNotFound for a missing key.
	//
	// Distinct from Open because some future drivers (e.g.
	// streaming pipes from a remote object store) can satisfy
	// the cheap Open contract but not Seek; keeping them
	// separate lets non-HTTP consumers stay on Open.
	OpenSeekable(ctx context.Context, key string) (io.ReadSeekCloser, error)
}

// Info is the metadata Stat returns. Modtime is the upper
// bound the download handler hands to http.ServeContent for
// If-Modified-Since checks; storage-backend definition of
// "modtime" is up to the driver (local_fs uses the on-disk
// mtime; the memory driver tracks the last successful Put).
type Info struct {
	Size    int64
	ModTime time.Time
}

// ErrNotFound is the canonical "key absent" sentinel every
// driver returns from Open. Callers compare with
// `errors.Is(err, kvfs.ErrNotFound)`.
var ErrNotFound = notFoundError{}

type notFoundError struct{}

func (notFoundError) Error() string { return "kvfs: key not found" }

// BuildKey composes the final storage key from the bucket
// path, the object's logical key, and the sub-bucket
// configuration. The returned string is what callers pass to
// [Driver.PutFromTempFile] / [Driver.Open] / [Driver.Delete].
//
// Layout:
//
//	<bucketPath>/<2 hex>/<2 hex>/.../<objectKey>
//
// where the leading `2*depth` hex chars come from
// `sha256(objectKey)` and depth is derived from
// `expectedObjects` and `maxPerBucket` per [BucketDepth].
//
// `bucketPath` is normalised to start with one `/` and not
// end with one ("/avatars" stays as-is; "/avatars/" loses
// the trailing slash; "avatars" gains the leading one).
// Empty bucket path is rejected by the parser before reaching
// this helper; here we treat empty as "" (no prefix) for
// resilience.
func BuildKey(bucketPath, objectKey string, expectedObjects uint64, maxPerBucket uint32) string {
	depth := BucketDepth(expectedObjects, maxPerBucket)
	return BuildKeyAt(bucketPath, objectKey, depth)
}

// BuildKeyAt is the depth-explicit variant of BuildKey. Used
// by tests that want to pin the depth without going through
// the derivation. Normal call sites use [BuildKey].
func BuildKeyAt(bucketPath, objectKey string, depth int) string {
	prefix := normalisePrefix(bucketPath)
	if depth <= 0 {
		if prefix == "" {
			return objectKey
		}
		return prefix + "/" + objectKey
	}
	hash := sha256.Sum256([]byte(objectKey))
	hex := hex.EncodeToString(hash[:])
	var b strings.Builder
	if prefix != "" {
		b.WriteString(prefix)
		b.WriteByte('/')
	}
	for i := 0; i < depth; i++ {
		b.WriteString(hex[i*2 : i*2+2])
		b.WriteByte('/')
	}
	b.WriteString(objectKey)
	return b.String()
}

// BucketDepth returns the sub-bucket nesting depth derived
// from the expected total object count and the per-leaf-
// bucket cap. The formula is
//
//	depth = ceil(log_maxPerBucket(expectedObjects))
//
// with these edge cases:
//
//   - expectedObjects ≤ maxPerBucket → depth 0 (flat layout
//     under bucket_path).
//   - maxPerBucket ≤ 1 — pathological config; treat as
//     "use depth 1" so the caller still gets some
//     distribution. The parser already rejects this; the
//     guard here is defensive.
//   - expectedObjects == 0 — depth 0 (no objects ever, no
//     sub-bucketing needed).
//
// The hex sub-bucket alphabet is base-256 per byte but we
// emit 2 hex chars per level (= 8 bits = 256 possibilities
// per directory). The math caps at 16 levels (the full
// sha256 prefix is 64 hex chars / 32 bytes); past that the
// caller's expected count exceeds what a single hash can
// distribute uniformly and we return 16.
func BucketDepth(expectedObjects uint64, maxPerBucket uint32) int {
	if expectedObjects == 0 {
		return 0
	}
	if maxPerBucket == 0 || maxPerBucket == 1 {
		return 1
	}
	if expectedObjects <= uint64(maxPerBucket) {
		return 0
	}
	// log_maxPerBucket(expectedObjects) = log(expected) / log(max).
	// Each sub-bucket level multiplies bucket count by 256
	// (one byte of hash). To honour a tighter user-set
	// max_per_bucket (say 1000), we still allocate one level
	// per "bucket-multiplier worth of growth" — depth grows
	// fast enough to cap leaf counts at maxPerBucket.
	//
	// Concretely: capacity at depth d = maxPerBucket * 256^d.
	// Solve maxPerBucket * 256^d ≥ expectedObjects:
	//   256^d ≥ expectedObjects / maxPerBucket
	//   d ≥ log256(expectedObjects / maxPerBucket)
	const sub = 256.0
	ratio := float64(expectedObjects) / float64(maxPerBucket)
	d := int(math.Ceil(math.Log(ratio) / math.Log(sub)))
	// coverage-exempt: ratio > 1 here (expectedObjects > maxPerBucket
	// is checked above), so log(ratio) > 0 and ceil(...) >= 1 — the
	// floor guard can never fire; kept as a defensive invariant.
	if d < 1 {
		d = 1
	}
	// coverage-exempt: capacity per level is 256, and 256^8 already
	// exceeds the uint64 ceiling on expectedObjects, so the derived
	// depth tops out near 8 — d > 16 is mathematically unreachable
	// for any representable input.
	if d > 16 {
		d = 16
	}
	return d
}

// HashKey returns the canonical content-derived object key
// the gateway uses for uploaded files. The 64-char hex
// rendering of sha256 is human-friendly in `ls` listings
// and survives URL transport without escaping.
func HashKey(content []byte) string {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

// HashFromReader streams a reader and returns the sha256
// hex without buffering the whole body. Used by the
// multipart handler (Slice B) where the upload streams to a
// tmp file and the hash needs to be computed during the
// same pass.
func HashFromReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// normalisePrefix tidies the user-supplied bucket path:
//
//   - "" → "" (no prefix)
//   - "avatars" → "/avatars"
//   - "/avatars" → "/avatars"
//   - "/avatars/" → "/avatars"
//   - "/" → ""
func normalisePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

// formatBytes renders a byte count for diag messages. Not
// used by the runtime path; exported for the parser-side
// validation diagnostics that surface size-cap errors.
func formatBytes(n uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/GiB)
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/MiB)
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/KiB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Compile-time use of formatBytes so the symbol stays alive
// across package consumers that haven't started exposing
// size-cap diags yet.
var _ = formatBytes
