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
	"sync"
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

// Identical bodies must land in distinct objects. A content-derived key
// shared one object between requests, so a request that failed after its
// upload rolled back the object an earlier, committed request still
// referenced.
func TestProcessFilePart_IdenticalBodiesGetDistinctObjects(t *testing.T) {
	d := memdriver.New()
	cfg := sameBytesConfig(d)
	h1 := uploadSameBytes(t, cfg, "a.pdf")
	h2 := uploadSameBytes(t, cfg, "different-name.pdf")
	if h1 == h2 {
		t.Fatalf("identical uploads share handle %q", h1)
	}
	if got := len(d.Keys()); got != 2 {
		t.Errorf("driver keys = %d, want 2 (one object per upload)", got)
	}
	// The key still starts from the content hash, so it stays traceable.
	want := kvfs.HashKey([]byte("same-bytes"))
	for _, h := range []string{h1, h2} {
		if !strings.Contains(h, want+"-") {
			t.Errorf("handle %q does not carry the content hash %s", h, want)
		}
	}
}

// A failed request's rollback must not remove an object an earlier,
// committed request stored with the same bytes.
func TestRollbackUploads_KeepsAnotherRequestsIdenticalObject(t *testing.T) {
	d := memdriver.New()
	cfg := sameBytesConfig(d)
	committed := uploadSameBytes(t, cfg, "a.pdf")
	failed := uploadSameBytes(t, cfg, "a.pdf")

	restgw.RollbackUploads(context.Background(), map[string]kvfs.Driver{"c": d},
		[]restgw.UploadedObject{{Connection: "c", Handle: failed}})

	if !d.Has(committed) {
		t.Fatalf("rollback of a failed request removed the committed request's object %q", committed)
	}
	if d.Has(failed) {
		t.Errorf("rollback left the failed request's own object %q behind", failed)
	}
}

// Two identical uploads in flight at once: whichever fails and rolls back,
// the other keeps its object.
func TestRollbackUploads_ConcurrentIdenticalUploads(t *testing.T) {
	d := memdriver.New()
	cfg := sameBytesConfig(d)
	const n = 16
	reqs := make([]*http.Request, n)
	for i := range reqs {
		reqs[i] = sameBytesRequest(t, "a.pdf")
	}
	handles := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			handles[i], errs[i] = restgw.ProcessFilePart(context.Background(), reqs[i], cfg)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}

	// Every odd request fails and rolls back concurrently.
	drivers := map[string]kvfs.Driver{"c": d}
	for i := 1; i < n; i += 2 {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			restgw.RollbackUploads(context.Background(), drivers, []restgw.UploadedObject{{Connection: "c", Handle: h}})
		}(handles[i])
	}
	wg.Wait()

	for i := 0; i < n; i += 2 {
		if !d.Has(handles[i]) {
			t.Errorf("request %d committed, but its object %q is gone", i, handles[i])
		}
	}
	if got := len(d.Keys()); got != n/2 {
		t.Errorf("driver keys = %d, want %d", got, n/2)
	}
}

func sameBytesConfig(d kvfs.Driver) restgw.FilePartConfig {
	return restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1000,
		MaxPerBucket:      10,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	}
}

// sameBytesRequest builds a parsed multipart request carrying the fixed
// body "same-bytes" under filename.
func sameBytesRequest(t *testing.T, filename string) *http.Request {
	t.Helper()
	body, ct := buildMultipart(t, map[string]string{"file": filename + "=same-bytes"})
	r := httptest.NewRequest(http.MethodPost, "/u", body)
	r.Header.Set("Content-Type", ct)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	return r
}

// uploadSameBytes stores the fixed body "same-bytes" under filename and
// returns the handle.
func uploadSameBytes(t *testing.T, cfg restgw.FilePartConfig, filename string) string {
	t.Helper()
	h, err := restgw.ProcessFilePart(context.Background(), sameBytesRequest(t, filename), cfg)
	if err != nil {
		t.Fatalf("ProcessFilePart: %v", err)
	}
	return h
}

// MaxSizeBytes == 0 means UNLIMITED, not "reject
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

// On the success path the staging tmp file must be
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
	// must have removed it. No w17-upload-* staging files may remain.
	leftovers, err := filepath.Glob(filepath.Join(tmpDir, "w17-upload-*"))
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
