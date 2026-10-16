package restgw_test

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// An UPLOADED_IMAGE field's original is served as the raster type its bytes
// are, so a browser renders it inline; what is not one of those — HTML, SVG,
// anything — stays application/octet-stream, whatever the uploaded name
// said. A field that is not an image serves every original as
// octet-stream, an image included. nosniff always.
func TestServeDownload_AnImageFieldsOriginalCarriesItsRasterType(t *testing.T) {
	var pngBytes, gifBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifBytes, image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	// The smallest WebP header http.DetectContentType recognises.
	webp := append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 24)...)

	d := memory.New()
	objects := map[string][]byte{
		"img/aa/png":  pngBytes.Bytes(),
		"img/aa/gif":  gifBytes.Bytes(),
		"img/aa/webp": webp,
		"img/aa/html": []byte("<html><script>alert(1)</script>"),
		"img/aa/svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"doc/aa/png":  pngBytes.Bytes(),
	}
	for k, v := range objects {
		d.PutBytes(k, v)
	}
	fields := []restgw.DownloadField{{BucketPath: "/img", Driver: d, Image: true}, {BucketPath: "/doc", Driver: d}}
	inline := restgw.NewDownloadConfig("/f", fields, "", false, false, false)
	attach := restgw.NewDownloadConfig("/f", fields, "", true, false, false)

	for _, tc := range []struct{ key, contentType, filename string }{
		{"img/aa/png", "image/png", "png.png"},
		{"img/aa/gif", "image/gif", "gif.gif"},
		{"img/aa/webp", "image/webp", "webp.webp"},
		{"img/aa/html", "application/octet-stream", "html"},
		{"img/aa/svg", "application/octet-stream", "svg"},
		{"doc/aa/png", "application/octet-stream", "png"}, // not an image field
	} {
		rec := get(inline, "/f/"+tc.key)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.key, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != tc.contentType {
			t.Errorf("%s: Content-Type %q, want %q", tc.key, ct, tc.contentType)
		}
		if ns := rec.Header().Get("X-Content-Type-Options"); ns != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q, want nosniff", tc.key, ns)
		}
		// The sniff rewinds: the whole object is served, from its first byte.
		if !bytes.Equal(rec.Body.Bytes(), objects[tc.key]) {
			t.Errorf("%s: body differs from the stored object (%d vs %d bytes)", tc.key, rec.Body.Len(), len(objects[tc.key]))
		}
		if cd := get(attach, "/f/"+tc.key).Header().Get("Content-Disposition"); cd != `attachment; filename="`+tc.filename+`"` {
			t.Errorf("%s: Content-Disposition %q, want filename %q", tc.key, cd, tc.filename)
		}
	}

	// A Range read of an image original is still the requested bytes.
	req := httptest.NewRequest(http.MethodGet, "/f/img/aa/png", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, req, inline)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), pngBytes.Bytes()[:4]) {
		t.Errorf("range of an image original: %d %q", rec.Code, rec.Body.Bytes())
	}
}
