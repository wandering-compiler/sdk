package restgw_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/imagevariant"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func variantDownload(t *testing.T) (*memory.Driver, restgw.DownloadConfig) {
	t.Helper()
	d := memory.New()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 800, 400))); err != nil {
		t.Fatal(err)
	}
	d.PutBytes("avatars/0123456789abcdef", b.Bytes())
	d.PutBytes("docs/deadbeef", []byte("<html><script>alert(1)</script>"))
	cfg := restgw.NewDownloadConfig("/api/files", []restgw.DownloadField{
		{BucketPath: "/avatars", Driver: d}, {BucketPath: "/docs", Driver: d},
	}, "public, max-age=31536000, immutable", false, false, false).
		WithVariants(imagevariant.Spec{Name: "thumb", MaxWidth: 200, MaxHeight: 200})
	return d, cfg
}

func get(cfg restgw.DownloadConfig, url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, url, nil), cfg)
	return rec
}

// A declared variant is the image fitted into its box, served as the image
// type it is, and stored beside the original: the second request reads the
// stored copy (it still answers after the original is gone).
func TestServeDownload_VariantIsRenderedOnceAndStored(t *testing.T) {
	d, cfg := variantDownload(t)
	rec := get(cfg, "/api/files/avatars/0123456789abcdef?variant=thumb")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if etag := rec.Header().Get("ETag"); etag != `"0123456789abcdef~thumb"` {
		t.Errorf("ETag = %q", etag)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	c, _, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || c.Width != 200 || c.Height != 100 {
		t.Fatalf("variant is %dx%d (%v), want 200x100", c.Width, c.Height, err)
	}
	if err := d.Delete(context.Background(), "avatars/0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if rec := get(cfg, "/api/files/avatars/0123456789abcdef?variant=thumb"); rec.Code != http.StatusOK {
		t.Errorf("the stored variant was not served once the original was gone: %d", rec.Code)
	}
}

func TestServeDownload_VariantRefusals(t *testing.T) {
	_, cfg := variantDownload(t)
	if rec := get(cfg, "/api/files/avatars/0123456789abcdef?variant=huge"); rec.Code != http.StatusBadRequest {
		t.Errorf("an undeclared variant: %d, want 400", rec.Code)
	}
	if rec := get(cfg, "/api/files/docs/deadbeef?variant=thumb"); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a variant of a non-image: %d, want 415", rec.Code)
	}
	if rec := get(cfg, "/api/files/avatars/nope?variant=thumb"); rec.Code != http.StatusNotFound {
		t.Errorf("a variant of a missing object: %d, want 404", rec.Code)
	}
	plain := restgw.NewDownloadConfig("/api/files", cfg.Fields, "", false, false, false)
	if rec := get(plain, "/api/files/avatars/0123456789abcdef?variant=thumb"); rec.Code != http.StatusBadRequest {
		t.Errorf("an endpoint without variants: %d, want 400", rec.Code)
	}
}
