package restgw_test

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	memdriver "github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func filePartRequest(t *testing.T, part string) *http.Request {
	t.Helper()
	body, contentType := buildMultipart(t, map[string]string{"file": part})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	return req
}

// E20.1 on disk: the refused part's staging file is gone, not only absent
// from the driver.
func TestProcessFilePart_TooLargeLeavesNoStagingFile(t *testing.T) {
	stage := t.TempDir()
	d := memdriver.New()
	_, err := restgw.ProcessFilePart(context.Background(), filePartRequest(t, "big.pdf="+strings.Repeat("X", 2048)), restgw.FilePartConfig{
		FormName: "file", Driver: d, TmpDir: stage, BucketPath: "/x",
		ExpectedObjects: 1, MaxPerBucket: 1, MaxSizeBytes: 1024, AllowedExtensions: []string{"pdf"},
	})
	if !errors.Is(err, restgw.ErrUploadTooLarge) {
		t.Fatalf("err = %v, want ErrUploadTooLarge", err)
	}
	entries, rerr := os.ReadDir(stage)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging dir holds %d file(s) after the refusal: %v", len(entries), entries)
	}
	if got := d.Keys(); len(got) != 0 {
		t.Fatalf("driver holds %v after the refusal", got)
	}
}

// A cap at or past math.MaxInt64 cannot be an int64 limit; it is no cap,
// and the whole body is stored (not an empty object from an overflowed,
// negative limit).
func TestProcessFilePart_MaxSizeAtTheInt64CeilingIsNoCap(t *testing.T) {
	for _, max := range []uint64{math.MaxInt64, math.MaxUint64} {
		d := memdriver.New()
		h, err := restgw.ProcessFilePart(context.Background(), filePartRequest(t, "doc.pdf=hello"), restgw.FilePartConfig{
			FormName: "file", Driver: d, TmpDir: t.TempDir(), BucketPath: "/x",
			ExpectedObjects: 1, MaxPerBucket: 1, MaxSizeBytes: max, AllowedExtensions: []string{"pdf"},
		})
		if err != nil {
			t.Fatalf("MaxSizeBytes=%d: %v", max, err)
		}
		rc, err := d.Open(context.Background(), h)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 16)
		n, _ := rc.Read(buf)
		_ = rc.Close()
		if string(buf[:n]) != "hello" {
			t.Fatalf("MaxSizeBytes=%d stored %q, want the whole body", max, buf[:n])
		}
	}
}
