package restgw_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/imagevariant"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// gatedDriver counts the renders in progress: a render begins with Open of
// the original (only a render opens the original on a variant request) and
// the reader it gets blocks until the test releases it.
type gatedDriver struct {
	*memory.Driver
	release          chan struct{}
	releaseOnce      sync.Once
	opens, now, peak atomic.Int64
}

// open lets every blocked and later render through.
func (d *gatedDriver) open() { d.releaseOnce.Do(func() { close(d.release) }) }

func (d *gatedDriver) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := d.Driver.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	d.opens.Add(1)
	n := d.now.Add(1)
	for {
		p := d.peak.Load()
		if n <= p || d.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-d.release
	return &countedReader{ReadCloser: rc, d: d}, nil
}

type countedReader struct {
	io.ReadCloser
	d    *gatedDriver
	once sync.Once
}

func (r *countedReader) Close() error {
	r.once.Do(func() { r.d.now.Add(-1) })
	return r.ReadCloser.Close()
}

var _ kvfs.Driver = (*gatedDriver)(nil)

func gatedVariants(t *testing.T, objects int) (*gatedDriver, restgw.DownloadConfig) {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 400, 200))); err != nil {
		t.Fatal(err)
	}
	d := &gatedDriver{Driver: memory.New(), release: make(chan struct{})}
	for i := range objects {
		d.PutBytes(fmt.Sprintf("img/%032x", i), b.Bytes())
	}
	t.Cleanup(d.open) // a failed test leaves no render blocked
	cfg := restgw.NewDownloadConfig("/files", []restgw.DownloadField{{
		BucketPath: "/img", Driver: d, Image: true,
		Variants: []imagevariant.Spec{{Name: "thumb", MaxWidth: 100}},
	}}, "", false, false, false)
	return d, cfg
}

// Concurrent first requests of the same variant MUST render it once: every
// request but one waits for that render and serves what it stored.
func TestServeDownload_ConcurrentRequestsOfOneVariantRenderOnce(t *testing.T) {
	d, cfg := gatedVariants(t, 1)
	const k = 8
	codes := make([]int, k)
	var wg sync.WaitGroup
	for i := range k {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = get(cfg, fmt.Sprintf("/files/img/%032x?variant=thumb", 0)).Code
		}()
	}
	time.Sleep(200 * time.Millisecond) // every request is in, waiting on the one render
	d.open()
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: %d, want 200", i, c)
		}
	}
	if n := d.opens.Load(); n != 1 {
		t.Fatalf("%d concurrent first requests rendered the variant %d times, want once", k, n)
	}
}

// Renders of different objects MUST NOT run more than the process bound at
// once: a decode costs up to the source's pixels times four bytes, so K
// concurrent first requests were K full decodes in memory together.
func TestServeDownload_ConcurrentVariantRendersAreBounded(t *testing.T) {
	defer restgw.SetMaxConcurrentVariantRenders(2)()
	d, cfg := gatedVariants(t, 8)
	codes := make([]int, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = get(cfg, fmt.Sprintf("/files/img/%032x?variant=thumb", i)).Code
		}()
	}
	time.Sleep(200 * time.Millisecond)
	if p := d.peak.Load(); p > 2 {
		t.Fatalf("%d renders ran at once, want at most 2", p)
	}
	d.open()
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: %d, want 200 (it waited for a slot)", i, c)
		}
	}
	if p := d.peak.Load(); p > 2 {
		t.Fatalf("%d renders ran at once, want at most 2", p)
	}
}

// A request that cannot get a render slot within the wait MUST be refused
// 503 with Retry-After rather than queue without bound, and so MUST every
// request waiting on that same variant.
func TestServeDownload_VariantRenderBusyIs503(t *testing.T) {
	defer restgw.SetMaxConcurrentVariantRenders(1)()
	defer restgw.SetVariantRenderWaitForTest(100 * time.Millisecond)()
	d, cfg := gatedVariants(t, 2)
	held := make(chan int, 1)
	go func() { held <- get(cfg, fmt.Sprintf("/files/img/%032x?variant=thumb", 0)).Code }()
	time.Sleep(50 * time.Millisecond) // the only slot is taken
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- get(cfg, fmt.Sprintf("/files/img/%032x?variant=thumb", 1)) }()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("a request with no render slot is still waiting after 5s; the wait is unbounded")
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("no slot: %d Retry-After=%q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	d.open()
	if c := <-held; c != http.StatusOK {
		t.Fatalf("the render holding the slot: %d", c)
	}
	if rec := get(cfg, fmt.Sprintf("/files/img/%032x?variant=thumb", 1)); rec.Code != http.StatusOK {
		t.Fatalf("once the slot is free: %d", rec.Code)
	}
}
