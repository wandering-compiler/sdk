// Package local_fs is the local-filesystem [kvfs.Driver]
// implementation. Production path for LOCAL_FS connections.
//
// PutFromTempFile is implemented via os.Rename — atomic on
// the same filesystem (POSIX rename). The caller is
// responsible for placing the tmp file on the same
// filesystem as the bucket root (the multipart handler in
// Slice B will use a tmp dir relative to the bucket root for
// exactly this reason; cross-fs renames fall back to copy +
// delete which is non-atomic).
//
// Sub-bucket directories materialise lazily — Put's
// MkdirAll covers any missing parent directories on the way
// to the final path. mkdirall mode is 0o755 by default;
// operators can mount the bucket root with whatever
// permission scheme they want and the file mode of newly
// created files is 0o644 (rw for owner, r for group +
// other) which matches typical web-hosted asset
// permissions.
package local_fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// Driver is the LOCAL_FS implementation. Constructed via
// [New] with the bucket root.
type Driver struct {
	root string
}

// errEscapesRoot is returned by [Driver.containedPath] when a key
// resolves outside the bucket root (a `..` traversal). Read paths
// translate it to [kvfs.ErrNotFound] so the boundary isn't leaked;
// write/delete paths propagate it as a hard reject.
var errEscapesRoot = errors.New("local_fs: key escapes bucket root")

// containedPath joins the bucket root with `key` and verifies the
// result stays inside the root. filepath.Join CLEANS `..` segments,
// so an un-sanitized key such as "avatars/../../../etc/passwd" would
// otherwise resolve to an absolute path outside d.root — an arbitrary
// file read/write/delete. Callers MUST route every key through here
// rather than calling filepath.Join directly.
func (d *Driver) containedPath(key string) (string, error) {
	final := filepath.Join(d.root, filepath.FromSlash(key))
	rel, err := filepath.Rel(d.root, final)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errEscapesRoot
	}
	return final, nil
}

// New returns a driver rooted at `root`. The root must
// already exist; failure to stat / mkdir at startup is
// caller's responsibility (the gateway main.go calls
// [EnsureRoot] before constructing the driver).
func New(root string) *Driver {
	return &Driver{root: root}
}

// EnsureRoot creates the bucket root if it doesn't exist.
// Idempotent. Permission errors propagate verbatim — the
// caller (gateway main.go) decides whether to fatal or
// continue.
func EnsureRoot(root string) error {
	if root == "" {
		return errors.New("local_fs: bucket root cannot be empty")
	}
	return os.MkdirAll(root, 0o755)
}

// PutFromTempFile atomically renames tmpPath to the resolved
// final path under the bucket root. Caller produces
// `key` via [kvfs.BuildKey] so sub-bucketing applies.
//
// Returns `key` unchanged — matches the [kvfs.Driver]
// contract ("handle is normally equal to key"). The DB /
// business layer stores the deployment-independent key, NOT
// the local FS path; downstream code opens the file via
// [Driver.Open] which re-joins the key with the driver's
// root at read time.
//
// On rename failure: if the underlying error is "cross-
// device link" we fall back to copy + delete to handle the
// case where the tmp file lives on a different filesystem
// than the bucket root (uncommon — Slice B's handler keeps
// the tmp file under the bucket root specifically to avoid
// this — but worth covering for robustness).
func (d *Driver) PutFromTempFile(_ context.Context, key string, tmpPath string) (string, error) {
	if d.root == "" {
		return "", errors.New("local_fs: driver not initialised (root is empty)")
	}
	if key == "" {
		return "", errors.New("local_fs: key cannot be empty")
	}
	final, err := d.containedPath(key)
	if err != nil {
		return "", fmt.Errorf("local_fs: put %q: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", fmt.Errorf("local_fs: mkdir parent: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		if errors.Is(err, errEXDEV) || isCrossDeviceErr(err) {
			if cpErr := copyFile(tmpPath, final); cpErr != nil {
				return "", fmt.Errorf("local_fs: cross-fs put fallback: %w", cpErr)
			}
			_ = os.Remove(tmpPath)
		} else {
			return "", fmt.Errorf("local_fs: rename %s → %s: %w", tmpPath, final, err)
		}
	}
	return key, nil
}

// Open returns a streaming reader for the stored object.
// Returns [kvfs.ErrNotFound] for a missing path.
func (d *Driver) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if d.root == "" {
		return nil, errors.New("local_fs: driver not initialised (root is empty)")
	}
	final, err := d.containedPath(key)
	if err != nil {
		// Escape attempt: report as missing, don't leak the boundary.
		return nil, kvfs.ErrNotFound
	}
	f, err := os.Open(final)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, kvfs.ErrNotFound
		}
		return nil, fmt.Errorf("local_fs: open %s: %w", final, err)
	}
	return f, nil
}

// Stat returns size + modtime for the on-disk file.
// Returns [kvfs.ErrNotFound] when the key is missing.
func (d *Driver) Stat(_ context.Context, key string) (kvfs.Info, error) {
	if d.root == "" {
		return kvfs.Info{}, errors.New("local_fs: driver not initialised (root is empty)")
	}
	final, err := d.containedPath(key)
	if err != nil {
		return kvfs.Info{}, kvfs.ErrNotFound
	}
	fi, err := os.Stat(final)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return kvfs.Info{}, kvfs.ErrNotFound
		}
		return kvfs.Info{}, fmt.Errorf("local_fs: stat %s: %w", final, err)
	}
	return kvfs.Info{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// OpenSeekable returns the file as an [io.ReadSeekCloser].
// `*os.File` already satisfies the contract directly.
// Returns [kvfs.ErrNotFound] when the key is missing.
func (d *Driver) OpenSeekable(_ context.Context, key string) (io.ReadSeekCloser, error) {
	if d.root == "" {
		return nil, errors.New("local_fs: driver not initialised (root is empty)")
	}
	final, err := d.containedPath(key)
	if err != nil {
		return nil, kvfs.ErrNotFound
	}
	f, err := os.Open(final)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, kvfs.ErrNotFound
		}
		return nil, fmt.Errorf("local_fs: open %s: %w", final, err)
	}
	return f, nil
}

// Delete removes the file. Idempotent on missing keys.
func (d *Driver) Delete(_ context.Context, key string) error {
	if d.root == "" {
		return errors.New("local_fs: driver not initialised (root is empty)")
	}
	final, err := d.containedPath(key)
	if err != nil {
		return fmt.Errorf("local_fs: delete %q: %w", key, err)
	}
	err = os.Remove(final)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("local_fs: remove %s: %w", final, err)
	}
	return nil
}

// copyFile is the cross-filesystem rename fallback. Streams
// in 32 KiB chunks so the copy never doubles memory usage
// regardless of file size.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) // #nosec G302 -- public blob store (e.g. avatars served to clients); files are intentionally world-readable, never secrets
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// Compile-time check that Driver implements kvfs.Driver.
var _ kvfs.Driver = (*Driver)(nil)
