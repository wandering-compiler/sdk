package kvfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Placement is where a field's files go on its connection: the field's
// `(w17.field).upload` knobs, as the code generator bakes them for the
// tier that writes the connection.
//
// It is the WHOLE input of the key derivation. A writer that holds the
// field's Placement and calls [Put] lays files out exactly as every other
// writer of that field would — which is what lets a connection's one
// writer be the gateway (an upload) or the business tier (a file it made)
// without either reproducing the other's layout.
type Placement struct {
	// BucketPath is the field's `bucket_path`, e.g. "/invoices/pdf".
	BucketPath string
	// ExpectedObjects and MaxPerBucket drive the sub-bucket depth
	// ([BucketDepth]).
	ExpectedObjects uint64
	MaxPerBucket    uint32
	// MaxSizeBytes caps the body; 0 = no cap.
	MaxSizeBytes uint64
}

// ErrTooLarge is returned by [Put] when the body exceeds
// Placement.MaxSizeBytes. Nothing is stored.
var ErrTooLarge = errors.New("kvfs: object exceeds the field's max_size_bytes")

// Put stores body as a new object of the field p describes and returns its
// handle — the value the field's column holds, which [Reader.Open] and the
// download endpoints take back verbatim.
//
// The body streams to a staging file (inside the driver's own storage when
// it is a [StagingDirProvider], so the publish is a same-filesystem rename;
// os.TempDir otherwise) while its sha256 is computed, then lands under
// [BuildKey](p.BucketPath, [ObjectKey](hash), …) in one PutFromTempFile.
// Every call stores its OWN object, even for a body identical to one
// already stored (see [ObjectKey]).
func Put(ctx context.Context, d Driver, p Placement, body io.Reader) (handle string, err error) {
	stageDir := ""
	if sp, ok := d.(StagingDirProvider); ok {
		if stageDir, err = sp.StagingDir(); err != nil {
			return "", fmt.Errorf("kvfs: driver staging dir: %w", err)
		}
	}
	tmpPath, hash, err := StageTempFile(body, stageDir, p.MaxSizeBytes)
	if err != nil {
		return "", err
	}
	// PutFromTempFile renames the file away on success; a driver that
	// copies instead (another filesystem, S3) leaves it, and an error
	// leaves it too. Either way it is ours to remove.
	defer func() { _ = os.Remove(tmpPath) }()
	objectKey, err := ObjectKey(hash)
	if err != nil {
		return "", err
	}
	key := BuildKey(p.BucketPath, objectKey, p.ExpectedObjects, p.MaxPerBucket)
	handle, err = d.PutFromTempFile(ctx, key, tmpPath)
	if err != nil {
		return "", fmt.Errorf("kvfs: driver put: %w", err)
	}
	return handle, nil
}

// ObjectKey names one stored object: the sha256 of its body followed by a
// random 128-bit suffix.
//
// Every write gets its OWN object, even when its bytes match an object that
// already exists. A purely content-derived key made identical uploads share
// one object, and nothing tracks how many rows reference it: a request that
// failed after its upload rolled back (Delete) the object a different,
// committed request still pointed at, and an application deleting the file
// of one row removed it for every other row with the same content.
// Per-write keys make "delete what this request stored" mean exactly that,
// including for two identical writes in flight at once. The cost is that
// identical bodies are stored once per write instead of once in total.
//
// The hash prefix keeps the key traceable to its content; the suffix is what
// makes it unique.
func ObjectKey(hash string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("kvfs: object key nonce: %w", err)
	}
	return hash + "-" + hex.EncodeToString(nonce[:]), nil
}

// StageTempFile copies body to a fresh temp file under dir (os.TempDir when
// empty) while computing its sha256, and returns the file's path and the
// hash. maxSize > 0 caps the body: one byte more is [ErrTooLarge]. On error
// the temp file is already removed.
func StageTempFile(body io.Reader, dir string, maxSize uint64) (tmpPath, hashHex string, err error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("kvfs: mkdir staging dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "w17-upload-*")
	if err != nil {
		return "", "", fmt.Errorf("kvfs: create staging file: %w", err)
	}
	tmpPath = tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	// LimitReader at max+1: a source with more than max bytes shows the
	// overflow as the extra byte.
	// A cap past math.MaxInt64-1 would overflow the int64 limit into a
	// negative one, which reads nothing and stores an empty object; no body
	// reaches it, so it is no cap.
	if maxSize >= math.MaxInt64 {
		maxSize = 0
	}
	src := body
	if maxSize > 0 {
		src = io.LimitReader(body, int64(maxSize)+1)
	}
	hashHex, n, err := HashFromReader(io.TeeReader(src, tmp))
	if err != nil {
		cleanup()
		return "", "", fmt.Errorf("kvfs: stream body: %w", err)
	}
	if maxSize > 0 && uint64(n) > maxSize {
		cleanup()
		return "", "", fmt.Errorf("%w (limit=%d bytes)", ErrTooLarge, maxSize)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", "", fmt.Errorf("kvfs: close staging file: %w", err)
	}
	return tmpPath, hashHex, nil
}
