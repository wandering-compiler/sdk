// REST → KV upload helper (Slice B of the file-upload
// feature). The generated multipart handler resolves each
// FILE_PART binding through ProcessFilePart, which:
//
//   1. Pulls the named multipart form file from the request.
//      The caller has already invoked ParseMultipartForm,
//      which means the part is buffered (in memory up to the
//      max-memory cap, then in a temp file). The aggregate
//      request body is itself bounded upstream by
//      http.MaxBytesReader in the generated multipart prelude
//      (sum of the endpoint's per-file caps + framing
//      headroom), so ParseMultipartForm cannot spill an
//      unbounded part to disk before this helper runs.
//   2. Re-streams the buffered part into a tmp file rooted in
//      the driver's bucket directory, hashing during write.
//      The copy is wrapped in io.LimitReader so an individual
//      part exceeding its own MaxSizeBytes cap is rejected
//      (ErrUploadTooLarge) — a tighter, per-field bound than
//      the body-level MaxBytesReader, which only caps the sum.
//   3. Validates the extension against the allowlist.
//   4. Computes the hash-derived sub-bucket path.
//   5. Atomically moves the tmp file to its final location
//      via the driver's PutFromTempFile primitive.
//   6. Returns the resolved storage handle for the gateway
//      to write into the request msg's UPLOADED_* field.
//
// Errors translate to canonical HTTP statuses by the caller:
// ErrFilePartMissing → 400, ErrUploadTooLarge → 413,
// ErrUploadBadExt → 400. Wrap with status.Errorf at the
// handler emit level; this helper stays HTTP-status-free
// so it composes cleanly with non-REST callers.

package restgw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// FilePartConfig captures one FILE_PART binding's runtime
// shape — populated by the generated handler from the
// parser's resolved (w17.field).upload submsg.
type FilePartConfig struct {
	// FormName is the multipart part name the gateway looks
	// for in the request body (per the binding's http_name).
	FormName string

	// Driver is the KV driver backing the upload's
	// connection. Constructed at gateway main.go boot time
	// from the connection's dialect-specific config (path
	// for LOCAL_FS, bucket + creds for S3, …).
	Driver kvfs.Driver

	// TmpDir is where the streaming write lands before the
	// atomic move into final storage. Convention: a
	// subdirectory under the driver's bucket root so the
	// rename stays within one filesystem (POSIX rename is
	// atomic on the same fs only). Empty falls back to
	// os.TempDir() with a copy+delete fallback in the
	// driver — slower but functional.
	TmpDir string

	// BucketPath is the per-field base path (e.g. "/avatars",
	// "/uploads/tasks"). Final keys live under this prefix
	// after the hash-based sub-bucketing util runs.
	BucketPath string

	// ExpectedObjects + MaxPerBucket drive the sub-bucket
	// depth derivation per kvfs.BuildKey.
	ExpectedObjects uint64
	MaxPerBucket    uint32

	// MaxSizeBytes caps the per-file payload. Streaming
	// reader wraps with io.LimitReader; uploads exceeding
	// the cap return ErrUploadTooLarge. A value of 0 means
	// UNLIMITED (no per-file cap) — note the generated gateway
	// never sets 0 (the migrator IR rejects an upload field
	// without a positive max_size_bytes), so 0 only arises for
	// direct non-REST callers that deliberately opt out.
	MaxSizeBytes uint64

	// AllowedExtensions is the lowercase, no-dot list of
	// suffixes the upload's filename must match. Special
	// value "*" disables the check (caller documents
	// "any extension is fine"). An empty list rejects every
	// upload (UPLOADED_FILE without extensions is rejected
	// at parse time anyway, so this is a defensive guard).
	AllowedExtensions []string
}

// ErrFilePartMissing surfaces when the request body has no
// part matching FormName (the binding's form name).
var ErrFilePartMissing = errors.New("restgw: file part missing from multipart body")

// ErrUploadTooLarge surfaces when the streamed body exceeds
// the per-field max_size_bytes cap. Maps to HTTP 413.
var ErrUploadTooLarge = errors.New("restgw: uploaded file exceeds max size")

// ErrUploadBadExt surfaces when the uploaded filename's
// extension isn't in the allowlist.
var ErrUploadBadExt = errors.New("restgw: uploaded file extension not allowed")

// ProcessFilePart runs the full upload pipeline for one
// FILE_PART binding. Returns the storage handle on success.
// On any error the tmp file is unlinked best-effort.
func ProcessFilePart(ctx context.Context, r *http.Request, cfg FilePartConfig) (string, error) {
	if cfg.Driver == nil {
		return "", errors.New("restgw: FilePartConfig.Driver is nil")
	}
	if cfg.FormName == "" {
		return "", errors.New("restgw: FilePartConfig.FormName is empty")
	}

	file, header, err := r.FormFile(cfg.FormName)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return "", fmt.Errorf("%w: %q", ErrFilePartMissing, cfg.FormName)
		}
		return "", fmt.Errorf("restgw: form file %q: %w", cfg.FormName, err)
	}
	defer func() { _ = file.Close() }()

	if err := validateExtension(header, cfg.AllowedExtensions); err != nil {
		return "", err
	}

	tmpPath, hash, err := streamToTempFile(file, cfg.TmpDir, cfg.MaxSizeBytes)
	if err != nil {
		return "", err
	}

	finalKey := kvfs.BuildKey(cfg.BucketPath, hash, cfg.ExpectedObjects, cfg.MaxPerBucket)
	handle, err := cfg.Driver.PutFromTempFile(ctx, finalKey, tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("restgw: driver put: %w", err)
	}
	// Best-effort cleanup of the staging file. PutFromTempFile is a
	// rename when the tmp dir and the final store share a filesystem
	// (the TmpDir convention) — in that case tmpPath no longer exists
	// and Remove is a harmless no-op. But when the driver falls back to
	// copy+delete across filesystems (the empty-TmpDir path documented
	// on FilePartConfig.TmpDir), or any driver that copies rather than
	// moves, the staging file would otherwise leak. Errors are ignored:
	// the upload already succeeded, and a missing/renamed file is the
	// expected case.
	_ = os.Remove(tmpPath)
	return handle, nil
}

// streamToTempFile copies body to a fresh tmp file under
// tmpDir while computing its sha256. Enforces maxSize by
// wrapping the body with io.LimitReader+1; any extra byte
// surfaces as ErrUploadTooLarge.
//
// maxSize == 0 means UNLIMITED — no per-file cap is applied and
// the whole part is streamed. The REST gateway never hits this
// path: the migrator IR build rejects an upload field whose
// max_size_bytes is unset / 0, so a generated FilePartConfig
// always carries a positive cap. The unlimited branch exists for
// direct non-REST callers of this package that deliberately omit
// a cap; treating 0 as "reject everything" (the old LimitReader
// +1 == 1-byte-limit behaviour) was a foot-gun that bounced even
// a 1-byte upload.
//
// Caller closes body. Returns (tmpPath, hashHex, err); on
// error the tmp file is unlinked.
func streamToTempFile(body io.Reader, tmpDir string, maxSize uint64) (string, string, error) {
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", "", fmt.Errorf("restgw: mkdir tmp: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "wc-upload-*")
	if err != nil {
		return "", "", fmt.Errorf("restgw: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	// LimitReader cap = max+1; if the source had more bytes
	// than max, the +1 catches the overflow. Standard
	// trick. maxSize == 0 disables the cap (unlimited) — see
	// the doc comment above.
	src := body
	if maxSize > 0 {
		src = io.LimitReader(body, int64(maxSize)+1)
	}
	hashHex, n, err := kvfs.HashFromReader(io.TeeReader(src, tmp))
	if err != nil {
		cleanup()
		return "", "", fmt.Errorf("restgw: stream upload: %w", err)
	}
	if maxSize > 0 && uint64(n) > maxSize {
		cleanup()
		return "", "", fmt.Errorf("%w (limit=%d bytes)", ErrUploadTooLarge, maxSize)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", "", fmt.Errorf("restgw: close tmp: %w", err)
	}
	return tmpPath, hashHex, nil
}

// validateExtension matches the multipart filename against
// the allowlist. The "*" wildcard short-circuits the check.
// Comparison is case-insensitive on the suffix portion.
//
// Files without an extension are rejected (returning a
// distinct error message via fmt.Errorf wrapping
// ErrUploadBadExt) when the allowlist is anything other
// than ["*"].
func validateExtension(header *multipart.FileHeader, allowed []string) error {
	for _, e := range allowed {
		if e == "*" {
			return nil
		}
	}
	if header == nil || header.Filename == "" {
		return fmt.Errorf("%w: filename missing on multipart part", ErrUploadBadExt)
	}
	name := strings.ToLower(filepath.Base(header.Filename))
	dot := strings.LastIndex(name, ".")
	if dot < 0 || dot == len(name)-1 {
		return fmt.Errorf("%w: filename %q has no extension", ErrUploadBadExt, header.Filename)
	}
	got := name[dot+1:]
	for _, e := range allowed {
		if strings.EqualFold(got, e) {
			return nil
		}
	}
	return fmt.Errorf("%w: filename %q (extension %q not in allowlist %v)", ErrUploadBadExt, header.Filename, got, allowed)
}

// WriteUploadError translates the upload-helper errors to
// canonical REST status responses. Generated handler emit
// calls this at the error site so the wire shape stays
// consistent with the rest of the gateway error envelope.
func WriteUploadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrFilePartMissing):
		WriteError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	case errors.Is(err, ErrUploadTooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, "RESOURCE_EXHAUSTED", err.Error())
	case errors.Is(err, ErrUploadBadExt):
		WriteError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	default:
		// Q36-restgw-1: don't reflect the internal error to the client.
		// The non-sentinel errors reaching here wrap driver / tmp-file
		// failures that carry server topology — S3 endpoint / bucket /
		// credentials (`restgw: driver put: …`) or absolute FS paths
		// (`restgw: mkdir/create/close tmp: …`). Log it and return a
		// generic message, matching WriteGRPCError / ServeDownload
		// (restgw-sec-3).
		observx.ReportError(context.Background(), err)
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}
