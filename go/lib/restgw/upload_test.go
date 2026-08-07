package restgw_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	memdriver "github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func TestProcessFilePart_HappyPath(t *testing.T) {
	d := memdriver.New()
	body, contentType := buildMultipart(t, map[string]string{
		"file": "alice.pdf=hello world",
		"meta": "json={\"x\":1}",
	})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}

	handle, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/uploads",
		ExpectedObjects:   1000,
		MaxPerBucket:      1000,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	})
	if err != nil {
		t.Fatalf("ProcessFilePart: %v", err)
	}
	// Handle should equal the resolved kvfs key (memory
	// driver returns key as handle).
	if handle == "" {
		t.Errorf("handle empty")
	}
	if !d.Has(handle) {
		t.Errorf("driver missing key %q after Put; keys=%v", handle, d.Keys())
	}
	rc, err := d.Open(context.Background(), handle)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello world" {
		t.Errorf("stored body = %q, want %q", got, "hello world")
	}
}

func TestProcessFilePart_MissingPart(t *testing.T) {
	d := memdriver.New()
	body, contentType := buildMultipart(t, map[string]string{
		"other": "x.pdf=ignored",
	})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	_, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"*"},
	})
	if !errors.Is(err, restgw.ErrFilePartMissing) {
		t.Errorf("err = %v, want ErrFilePartMissing", err)
	}
}

func TestProcessFilePart_TooLarge(t *testing.T) {
	d := memdriver.New()
	big := strings.Repeat("X", 2048)
	body, contentType := buildMultipart(t, map[string]string{
		"file": "big.pdf=" + big,
	})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	_, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	})
	if !errors.Is(err, restgw.ErrUploadTooLarge) {
		t.Errorf("err = %v, want ErrUploadTooLarge", err)
	}
	// Driver must be empty — the cap fires before Put.
	if got := d.Keys(); len(got) != 0 {
		t.Errorf("driver should be empty on size-cap rejection; got %v", got)
	}
}

func TestProcessFilePart_BadExtension(t *testing.T) {
	d := memdriver.New()
	body, contentType := buildMultipart(t, map[string]string{
		"file": "bad.exe=hostile",
	})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	_, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	})
	if !errors.Is(err, restgw.ErrUploadBadExt) {
		t.Errorf("err = %v, want ErrUploadBadExt", err)
	}
}

func TestProcessFilePart_WildcardExt(t *testing.T) {
	d := memdriver.New()
	body, contentType := buildMultipart(t, map[string]string{
		"file": "anything.weirdext=ok",
	})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	if _, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"*"},
	}); err != nil {
		t.Fatalf("wildcard upload: %v", err)
	}
}

func TestProcessFilePart_HashStable(t *testing.T) {
	// Same body → same hash → same final key, regardless of
	// uploaded filename. Confirms the storage handle is
	// content-derived (hash-keyed), so duplicate uploads
	// dedupe at the driver layer.
	d := memdriver.New()
	body1, ct1 := buildMultipart(t, map[string]string{"file": "a.pdf=same-bytes"})
	body2, ct2 := buildMultipart(t, map[string]string{"file": "different-name.pdf=same-bytes"})

	r1 := httptest.NewRequest(http.MethodPost, "/u", body1)
	r1.Header.Set("Content-Type", ct1)
	if err := r1.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm 1: %v", err)
	}
	r2 := httptest.NewRequest(http.MethodPost, "/u", body2)
	r2.Header.Set("Content-Type", ct2)
	if err := r2.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm 2: %v", err)
	}

	cfg := restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	}
	h1, err := restgw.ProcessFilePart(context.Background(), r1, cfg)
	if err != nil {
		t.Fatalf("Process 1: %v", err)
	}
	h2, err := restgw.ProcessFilePart(context.Background(), r2, cfg)
	if err != nil {
		t.Fatalf("Process 2: %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash-keyed handles differ: %q vs %q (same body)", h1, h2)
	}
	// Both Puts under the same key → only one entry in the
	// driver.
	if got := len(d.Keys()); got != 1 {
		t.Errorf("driver keys = %d, want 1 (dedup by hash)", got)
	}
}

// R-restgw-5 — MaxSizeBytes == 0 means UNLIMITED, not "reject
// everything". The old LimitReader+1 trick turned a 0 cap into a
// 1-byte limit that bounced even a 1-byte upload; the fix skips
// the cap entirely for 0. (The generated gateway never hits this
// — the migrator IR requires a positive cap — but the public
// helper must behave sanely for direct callers.)
func TestProcessFilePart_MaxSizeZero_Unlimited(t *testing.T) {
	d := memdriver.New()
	body, contentType := buildMultipart(t, map[string]string{
		"file": "doc.pdf=" + strings.Repeat("Z", 4096),
	})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	handle, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      0, // unlimited
		AllowedExtensions: []string{"pdf"},
	})
	if err != nil {
		t.Fatalf("ProcessFilePart with maxSize=0 should accept: %v", err)
	}
	if !d.Has(handle) {
		t.Errorf("driver missing key %q; keys=%v", handle, d.Keys())
	}
}

// R-restgw-5 — on the success path the staging tmp file must be
// removed even when the driver COPIES rather than renames (the
// cross-filesystem / non-rename driver path). Otherwise every
// upload leaks a file under TmpDir. copyLeaveDriver mimics a
// copying driver that leaves the tmp file behind; ProcessFilePart
// must clean it up.
func TestProcessFilePart_RemovesTempFileOnCopyDriver(t *testing.T) {
	tmpDir := t.TempDir()
	d := &copyLeaveDriver{Driver: memdriver.New()}
	body, contentType := buildMultipart(t, map[string]string{
		"file": "doc.pdf=payload-bytes",
	})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	handle, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		TmpDir:            tmpDir,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	})
	if err != nil {
		t.Fatalf("ProcessFilePart: %v", err)
	}
	if !d.Has(handle) {
		t.Errorf("driver missing key %q after copy Put", handle)
	}
	// The copying driver deliberately left the tmp file; the helper
	// must have removed it. No wc-upload-* staging files may remain.
	leftovers, err := filepath.Glob(filepath.Join(tmpDir, "wc-upload-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("tmp files leaked after successful copy-Put: %v", leftovers)
	}
}

// copyLeaveDriver embeds the memory driver but overrides
// PutFromTempFile to COPY the bytes and intentionally leave the
// tmp file in place — simulating a driver that can't rename (e.g.
// tmp and final store on different filesystems).
type copyLeaveDriver struct {
	*memdriver.Driver
}

func (c *copyLeaveDriver) PutFromTempFile(_ context.Context, key string, tmpPath string) (string, error) {
	b, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}
	c.PutBytes(key, b)
	// Intentionally do NOT remove tmpPath — that's the leak
	// ProcessFilePart's post-success cleanup must cover.
	return key, nil
}

// buildMultipart synthesises a multipart/form-data body
// from a "name → 'filename=body'" map. Used by the upload
// tests; keeps the per-test setup compact.
func buildMultipart(t *testing.T, parts map[string]string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, spec := range parts {
		eq := strings.Index(spec, "=")
		if eq < 0 {
			t.Fatalf("bad part spec %q", spec)
		}
		filename := spec[:eq]
		body := spec[eq+1:]
		if filename == "json" {
			// Plain text/value form field, not file.
			if err := mw.WriteField(name, body); err != nil {
				t.Fatalf("WriteField: %v", err)
			}
			continue
		}
		w, err := mw.CreateFormFile(name, filename)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// kvfs alias check — keeps the import-graph test ergonomic.
var _ = kvfs.ErrNotFound
