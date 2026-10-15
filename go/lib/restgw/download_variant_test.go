package restgw_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/imagevariant"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
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
	thumb := []imagevariant.Spec{{Name: "thumb", MaxWidth: 200, MaxHeight: 200}}
	cfg := restgw.NewDownloadConfig("/api/files", []restgw.DownloadField{
		{BucketPath: "/avatars", Driver: d, Variants: thumb}, {BucketPath: "/docs", Driver: d, Variants: thumb},
	}, "public, max-age=31536000, immutable", false, false, false)
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
	plain := restgw.NewDownloadConfig("/api/files", []restgw.DownloadField{{BucketPath: "/avatars", Driver: cfg.Fields[0].Driver}}, "", false, false, false)
	if rec := get(plain, "/api/files/avatars/0123456789abcdef?variant=thumb"); rec.Code != http.StatusBadRequest {
		t.Errorf("an endpoint without variants: %d, want 400", rec.Code)
	}
}

// A succeeded request's uploads go to the workers, which render every
// declared variant into the store: the first viewer then finds it there.
func TestVariantWorkers_RenderWhatASucceededRequestQueued(t *testing.T) {
	d, _ := variantDownload(t)
	ctx, cancel := context.WithCancel(context.Background())
	q := restgw.StartVariantWorkers(ctx, 2, func(err error) { t.Errorf("render: %v", err) })
	specs := []imagevariant.Spec{{Name: "thumb", MaxWidth: 200}, {Name: "card", MaxWidth: 400}}
	restgw.QueueVariants(q, map[string]kvfs.Driver{"up": d}, []restgw.UploadedObject{
		{Connection: "up", Handle: "avatars/0123456789abcdef", Variants: specs, Pregenerate: true},
		{Connection: "up", Handle: "avatars/0123456789abcdef", Variants: specs[:1], Pregenerate: false}, // queues nothing
	})
	deadline := time.Now().Add(10 * time.Second)
	for _, name := range []string{"thumb", "card"} {
		for {
			if _, err := d.Stat(ctx, restgw.VariantKey("avatars/0123456789abcdef", name)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("variant %s was never rendered", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	q.Wait()
}

// A full queue drops the job instead of blocking the upload: the download
// renders a missing variant on the spot.
func TestVariantWorkers_AFullQueueDropsRatherThanBlocks(t *testing.T) {
	d, _ := variantDownload(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // workers stop at once; nothing drains the queue
	q := restgw.StartVariantWorkers(ctx, 1, nil)
	q.Wait()
	job := restgw.VariantJob{Driver: d, Key: "avatars/0123456789abcdef", Variants: []imagevariant.Spec{{Name: "t", MaxWidth: 10}}}
	accepted := 0
	for i := 0; i < 2000; i++ {
		if q.Enqueue(job) {
			accepted++
		}
	}
	if accepted == 2000 || q.Dropped() == 0 {
		t.Errorf("accepted %d of 2000, dropped %d — a bounded queue never refused", accepted, q.Dropped())
	}
	var none *restgw.VariantWorkers
	if none.Enqueue(job) {
		t.Error("a nil queue (background rendering off) accepted a job")
	}
}

func TestRollbackUploads_DeletesTheVariantsToo(t *testing.T) {
	d, cfg := variantDownload(t)
	if rec := get(cfg, "/api/files/avatars/0123456789abcdef?variant=thumb"); rec.Code != http.StatusOK {
		t.Fatalf("render: %d", rec.Code)
	}
	restgw.RollbackUploads(context.Background(), map[string]kvfs.Driver{"up": d}, []restgw.UploadedObject{
		{Connection: "up", Handle: "avatars/0123456789abcdef", Variants: []imagevariant.Spec{{Name: "thumb"}}},
	})
	if _, err := d.Stat(context.Background(), restgw.VariantKey("avatars/0123456789abcdef", "thumb")); err == nil {
		t.Error("the rolled-back object's variant is still stored")
	}
}

func TestVariantWorkersFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if n, err := restgw.VariantWorkersFromEnv("GW", get); err != nil || n != restgw.DefaultVariantWorkers {
		t.Errorf("unset: %d %v", n, err)
	}
	env["GW_VARIANT_WORKERS"] = "0"
	if n, err := restgw.VariantWorkersFromEnv("GW", get); err != nil || n != 0 {
		t.Errorf("0: %d %v", n, err)
	}
	env["GW_VARIANT_WORKERS"] = "many"
	if _, err := restgw.VariantWorkersFromEnv("GW", get); err == nil {
		t.Error("a non-number was accepted")
	}
}
