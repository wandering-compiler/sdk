package restgw_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// newTestDownload seeds a memory-backed kvfs driver with two
// objects under two distinct bucket paths and returns the
// composed DownloadConfig the runtime helper consumes.
func newTestDownload(t *testing.T) (*memory.Driver, restgw.DownloadConfig) {
	t.Helper()
	d := memory.New()
	frozen := time.Date(2026, 5, 8, 9, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return frozen })
	d.PutBytes("avatars/0123456789abcdef", []byte("AVATAR-BODY"))
	d.PutBytes("docs/deadbeef", []byte("0123456789ABCDEF"))
	cfg := restgw.NewDownloadConfig(
		"/api/files/static",
		[]restgw.DownloadField{
			{BucketPath: "/avatars", Driver: d},
			{BucketPath: "/docs", Driver: d},
		},
		"public, max-age=3600",
		false, // attachment off
		false, // etag on
		false, // range on
	)
	return d, cfg
}

// TestServeDownload_NoContentSniffXSS — Q36-restgw-2. Uploaded bytes are
// served same-origin and the upload allowlist checks only the filename
// extension, not the content. An object whose body is HTML must come back
// as a non-renderable type + nosniff, NOT text/html — otherwise a
// same-origin inline download executes the markup (stored XSS).
func TestServeDownload_NoContentSniffXSS(t *testing.T) {
	d := memory.New()
	d.PutBytes("avatars/evil", []byte("<html><body><script>alert(document.domain)</script></body></html>"))
	cfg := restgw.NewDownloadConfig(
		"/api/files/static",
		[]restgw.DownloadField{{BucketPath: "/avatars", Driver: d}},
		"", false, false, false,
	)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/avatars/evil", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q — uploaded HTML served as a renderable type (stored XSS)", ct)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if ns := rec.Header().Get("X-Content-Type-Options"); ns != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", ns)
	}
}

func TestServeDownload_ServesObject(t *testing.T) {
	_, cfg := newTestDownload(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/avatars/0123456789abcdef", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "AVATAR-BODY" {
		t.Errorf("body = %q, want AVATAR-BODY", got)
	}
	if etag := rec.Header().Get("ETag"); etag != `"0123456789abcdef"` {
		t.Errorf("ETag = %q", etag)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
		t.Errorf("Content-Disposition = %q, want inline", cd)
	}
}

// TestServeDownload_PathTraversal404 is the SEC-02 regression guard at
// the HTTP boundary. chi does not clean `..` segments, so a remainder
// like `avatars/../../../etc/passwd` must be rejected before it reaches
// the driver. 404 (not 403) preserves the no-leak posture.
func TestServeDownload_PathTraversal404(t *testing.T) {
	for _, target := range []string{
		"/api/files/static/avatars/../../../etc/passwd",
		"/api/files/static/avatars/..%2f..%2f..%2fetc/passwd", // (won't decode here, but must not 200)
		"/api/files/static/../avatars/0123456789abcdef",
		"/api/files/static/./avatars/0123456789abcdef",
	} {
		_, cfg := newTestDownload(t)
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		restgw.ServeDownload(rec, req, cfg)
		if rec.Code == http.StatusOK {
			t.Errorf("target %q returned 200; traversal must be rejected", target)
		}
	}
}

// R-restgw-6 — the traversal guard must be separator-agnostic.
// A '/'-only split lets a backslash-separated dot segment
// (`avatars\..\..\etc\passwd`) slip past on a host where '\' is
// also a path separator. The guard normalises '\' to '/' before
// scanning, so these must 404 (never 200).
func TestServeDownload_BackslashTraversal404(t *testing.T) {
	for _, target := range []string{
		`/api/files/static/avatars\..\..\..\etc\passwd`,
		`/api/files/static/avatars/..\..\secret`,
		`/api/files/static/..\avatars\0123456789abcdef`,
	} {
		_, cfg := newTestDownload(t)
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		restgw.ServeDownload(rec, req, cfg)
		if rec.Code == http.StatusOK {
			t.Errorf("target %q returned 200; backslash traversal must be rejected", target)
		}
	}
}

func TestServeDownload_IfNoneMatch_304(t *testing.T) {
	_, cfg := newTestDownload(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/avatars/0123456789abcdef", nil)
	req.Header.Set("If-None-Match", `"0123456789abcdef"`)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 body should be empty, got %d bytes", rec.Body.Len())
	}
}

func TestServeDownload_RangeRequest_206(t *testing.T) {
	_, cfg := newTestDownload(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/docs/deadbeef", nil)
	req.Header.Set("Range", "bytes=0-9")
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	got, _ := io.ReadAll(rec.Result().Body)
	if string(got) != "0123456789" {
		t.Errorf("body = %q, want 0123456789", got)
	}
	if cr := rec.Header().Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-9/") {
		t.Errorf("Content-Range = %q, want prefix bytes 0-9/", cr)
	}
}

func TestServeDownload_DisableRange_IgnoresRange(t *testing.T) {
	d, cfg := newTestDownload(t)
	cfg = restgw.NewDownloadConfig(cfg.URLPrefix, []restgw.DownloadField{
		{BucketPath: "/avatars", Driver: d},
		{BucketPath: "/docs", Driver: d},
	}, cfg.CacheControl, cfg.AsAttachment, cfg.DisableETag, true)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/docs/deadbeef", nil)
	req.Header.Set("Range", "bytes=0-9")
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Range disabled)", rec.Code)
	}
	got, _ := io.ReadAll(rec.Result().Body)
	if string(got) != "0123456789ABCDEF" {
		t.Errorf("body = %q, want full body", got)
	}
}

func TestServeDownload_Attachment(t *testing.T) {
	d, cfg := newTestDownload(t)
	cfg = restgw.NewDownloadConfig(cfg.URLPrefix, []restgw.DownloadField{
		{BucketPath: "/avatars", Driver: d},
		{BucketPath: "/docs", Driver: d},
	}, cfg.CacheControl, true, false, false)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/avatars/0123456789abcdef", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="0123456789abcdef"` {
		t.Errorf("Content-Disposition = %q, want attachment+filename", cd)
	}
}

func TestServeDownload_DisableETag(t *testing.T) {
	d, cfg := newTestDownload(t)
	cfg = restgw.NewDownloadConfig(cfg.URLPrefix, []restgw.DownloadField{
		{BucketPath: "/avatars", Driver: d},
		{BucketPath: "/docs", Driver: d},
	}, "", false, true, false)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/avatars/0123456789abcdef", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if etag := rec.Header().Get("ETag"); etag != "" {
		t.Errorf("ETag should be empty when DisableETag, got %q", etag)
	}
}

func TestServeDownload_NotFound(t *testing.T) {
	_, cfg := newTestDownload(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/avatars/missing", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestServeDownload_UnknownBucket_404(t *testing.T) {
	_, cfg := newTestDownload(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static/elsewhere/key", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (unknown bucket leaks no info)", rec.Code)
	}
}

func TestServeDownload_PrefixMissing_404(t *testing.T) {
	_, cfg := newTestDownload(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files/static", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no remainder)", rec.Code)
	}
}

func TestServeDownload_HEAD(t *testing.T) {
	_, cfg := newTestDownload(t)
	req := httptest.NewRequest(http.MethodHead, "/api/files/static/avatars/0123456789abcdef", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", rec.Body.Len())
	}
	if cl := rec.Header().Get("Content-Length"); cl != "11" {
		t.Errorf("Content-Length = %q, want 11", cl)
	}
}

func TestServeDownload_DELETE_405(t *testing.T) {
	_, cfg := newTestDownload(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/files/static/avatars/0123456789abcdef", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", rec.Header().Get("Allow"))
	}
}

func TestBuildDownloadURL_Relative(t *testing.T) {
	got := restgw.BuildDownloadURL("RELATIVE", "/api/files/static", "https://ignored.example", "avatars/abc/0123")
	want := "/api/files/static/avatars/abc/0123"
	if got != want {
		t.Errorf("got %q, want %q (host ignored on RELATIVE)", got, want)
	}
}

func TestBuildDownloadURL_Absolute(t *testing.T) {
	got := restgw.BuildDownloadURL("ABSOLUTE", "/api/files/static", "https://files.example.com", "avatars/abc/0123")
	want := "https://files.example.com/api/files/static/avatars/abc/0123"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildDownloadURL_TrimsHostTrailingSlash(t *testing.T) {
	got := restgw.BuildDownloadURL("ABSOLUTE", "/api/files/static", "https://files.example.com/", "k")
	want := "https://files.example.com/api/files/static/k"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildDownloadURL_EmptyKey(t *testing.T) {
	got := restgw.BuildDownloadURL("ABSOLUTE", "/api/files/static", "https://files.example.com", "")
	if got != "" {
		t.Errorf("empty storage key should yield empty URL, got %q", got)
	}
}

func TestBuildDownloadURL_AbsoluteEmptyHost_FallsBackToRelative(t *testing.T) {
	// Operator misconfig (host env var empty). Defensive — the
	// main.go boot check is the proper guard; here we emit a
	// path-only URL so a request doesn't outright crash.
	got := restgw.BuildDownloadURL("ABSOLUTE", "/api/files/static", "", "k")
	want := "/api/files/static/k"
	if got != want {
		t.Errorf("got %q, want %q (defensive fallback)", got, want)
	}
}

// Longer prefix wins when one bucket path is a prefix of
// another. Crafts /a + /a-bigger; remainder "a-bigger/key"
// must hit /a-bigger, not /a.
func TestServeDownload_LongestPrefixWins(t *testing.T) {
	d := memory.New()
	d.PutBytes("a/x", []byte("SHORT"))
	d.PutBytes("a-bigger/x", []byte("LONG"))
	cfg := restgw.NewDownloadConfig(
		"/static",
		[]restgw.DownloadField{
			{BucketPath: "/a", Driver: d},
			{BucketPath: "/a-bigger", Driver: d},
		},
		"", false, false, false,
	)
	req := httptest.NewRequest(http.MethodGet, "/static/a-bigger/x", nil)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, cfg)
	if got := rec.Body.String(); got != "LONG" {
		t.Errorf("body = %q, want LONG (longest-prefix match)", got)
	}
}
