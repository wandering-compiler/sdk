// Package local_fs is the local-filesystem [kvfs.Driver]
// implementation. Production path for LOCAL_FS connections.
//
// PutFromTempFile is implemented via os.Rename — atomic on
// the same filesystem (POSIX rename). Callers get that
// placement for free by asking [Driver.StagingDir] where to
// stage; the multipart handler does exactly that when its
// FilePartConfig.TmpDir is empty, which is what every
// generated gateway emits.
//
// A tmp file from ANOTHER filesystem still publishes
// atomically: the fallback copies into a temp file in the
// DESTINATION directory and renames it over the key. It never
// writes into the final path and never unlinks it on failure
// — a previously published object survives a failed put
// untouched (T3-7 pass #9, C-F4).
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
// device link" we fall back to a staged copy + delete for
// the case where the tmp file lives on a different
// filesystem than the bucket root. That is NOT an exotic
// case when a caller ignores [Driver.StagingDir]: a bucket
// root on a mounted volume and a tmp file under os.TempDir()
// are different filesystems on every containerised
// deployment, so the fallback is a first-class path and
// upholds the same atomicity contract as the rename.
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
	// Normalise the published mode. The staging file the caller hands
	// over is an os.CreateTemp (0o600) while the fallback publishes its
	// own staging file — without this the object's permissions would
	// depend on WHICH branch published it, and the package's documented
	// 0o644 ("typical web-hosted asset permissions") would hold only on
	// the cross-device path. Best-effort: the file was created by this
	// process moments ago, so a failure here is not a reason to report
	// an upload that did land as failed.
	_ = os.Chmod(final, objectMode)
	return key, nil
}

// objectMode is the published object's permission — rw for owner, r for
// group + other. Blobs in this store are public assets (avatars,
// attachments) commonly served by a separate static file server running
// as a different user; they are never secrets.
const objectMode = 0o644 // #nosec G302 -- public blob store, deliberately world-readable

// StagingDir returns a directory INSIDE the bucket root that callers
// should stream uploads into before handing the path to
// [Driver.PutFromTempFile]. Staging there makes the publish a
// same-filesystem os.Rename — the only branch that is atomic by
// construction — instead of the copy fallback.
//
// This exists because the convention documented on
// restgw.FilePartConfig.TmpDir ("a subdirectory under the driver's
// bucket root") cannot be honoured by a code GENERATOR: the bucket root
// is a runtime env value the gateway resolves at boot, so the only
// component that knows it is the driver itself. Advertising it here lets
// the upload handler get the placement right for every deployment,
// including every project already generated (T3-7 pass #9, C-F4).
//
// The directory is dot-prefixed so it never collides with a key built by
// [kvfs.BuildKey] and stays out of the way of an operator listing the
// bucket. It is created on demand; the caller removes its own files.
func (d *Driver) StagingDir() (string, error) {
	if d.root == "" {
		return "", errors.New("local_fs: driver not initialised (root is empty)")
	}
	dir := filepath.Join(d.root, stagingDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("local_fs: staging dir %s: %w", dir, err)
	}
	return dir, nil
}

// stagingDirName is the bucket-root-relative home for in-flight uploads.
const stagingDirName = ".w17-staging"

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

// copyFile is the cross-filesystem rename fallback. It
// streams (io.Copy's 32 KiB chunks, so memory never scales
// with the object) into a TEMP FILE IN THE DESTINATION
// DIRECTORY and publishes it with an os.Rename — never into
// `dst` itself.
//
// Writing straight into `dst` would break the two halves of
// the [kvfs.Driver] contract this driver documents:
//
//   - O_TRUNC on the final path exposes an EMPTY, then
//     partial, body to every concurrent reader for the whole
//     duration of the copy. Keys are content-addressed, so a
//     re-upload of bytes that already exist targets a LIVE
//     object other rows reference.
//   - unlinking `dst` when the copy fails DESTROYS that live
//     object: a disk-full mid-copy would 404 every row
//     pointing at the key, and only the uploader is told
//     anything went wrong.
//
// Staging in dst's own directory keeps the publish a
// same-filesystem rename by construction, and the failure
// path removes only the driver's own staging file.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := out.Name()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// Compile-time check that Driver implements kvfs.Driver.
var _ kvfs.Driver = (*Driver)(nil)
