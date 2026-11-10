package restgw_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	kvs3 "github.com/wandering-compiler/sdk/go/lib/kvfs/s3"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// fakeS3 serves one object path-style (/<bucket>/<key>) and counts the
// requests by method: HEAD answers the size, GET honours an open-ended Range.
type fakeS3 struct {
	mu    sync.Mutex
	body  []byte
	heads int
	gets  int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Last-Modified", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Format(http.TimeFormat))
	switch r.Method {
	case http.MethodHead:
		f.heads++
		w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
	case http.MethodGet:
		f.gets++
		from := 0
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			from, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(f.body)-1, len(f.body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(f.body)-from))
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write(f.body[from:])
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// Serving an image field's original from S3 asks the store once for its
// size and fetches the body once: the size ServeDownload already has is
// handed to the reader (no second HEAD), and the 512 bytes read to sniff
// the type are replayed instead of re-fetched after the rewind.
func TestServeDownload_AnS3ImageIsOneHeadAndOneGet(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 300, 300))); err != nil {
		t.Fatal(err)
	}
	store := &fakeS3{body: b.Bytes()}
	srv := httptest.NewServer(store)
	defer srv.Close()
	d, err := kvs3.New(context.Background(), "s3://AK:SK@bucket?region=us-east-1&endpoint="+srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := restgw.NewDownloadConfig("/api/files", []restgw.DownloadField{{BucketPath: "/avatars", Driver: d, Image: true}}, "", false, false, false)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/api/files/avatars/0123456789abcdef", nil), cfg)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), store.body) || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("status %d, %d bytes, type %q; want the whole PNG", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.heads != 1 || store.gets != 1 {
		t.Fatalf("one download cost %d HEAD and %d GET requests, want 1 and 1", store.heads, store.gets)
	}
}

// Ranges stay exact through the lazy reader: a range inside the sniffed
// head, one past it, and the whole object after the rewind.
func TestServeDownload_S3RangesAreExact(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 400, 400))); err != nil {
		t.Fatal(err)
	}
	for b.Len() < 4096 { // make the object longer than the head
		b.WriteByte(0)
	}
	store := &fakeS3{body: b.Bytes()}
	srv := httptest.NewServer(store)
	defer srv.Close()
	d, err := kvs3.New(context.Background(), "s3://AK:SK@bucket?region=us-east-1&endpoint="+srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := restgw.NewDownloadConfig("/api/files", []restgw.DownloadField{{BucketPath: "/avatars", Driver: d, Image: true}}, "", false, false, false)
	for _, c := range []struct {
		rng      string
		from, to int
	}{{"bytes=10-99", 10, 99}, {"bytes=600-1999", 600, 1999}, {"bytes=0-", 0, len(store.body) - 1}} {
		req := httptest.NewRequest(http.MethodGet, "/api/files/avatars/0123456789abcdef", nil)
		req.Header.Set("Range", c.rng)
		rec := httptest.NewRecorder()
		restgw.ServeDownload(rec, req, cfg)
		if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), store.body[c.from:c.to+1]) {
			t.Fatalf("%s: status %d, %d bytes; want 206 with bytes %d-%d", c.rng, rec.Code, rec.Body.Len(), c.from, c.to)
		}
	}
}
