package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// fakeStore answers just enough of the S3 wire for the driver's error
// mapping: a PUT succeeds, a HEAD and a GET of `missing` answer a bare 404
// (no body: what a HEAD always gets, and what a wrong endpoint or path
// answers a GET with), anything else 500.
func fakeStore(t *testing.T) *Driver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			w.Header().Set("ETag", `"x"`)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/missing"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	d, err := New(context.Background(), "s3://AKID:SECRET@bucket?endpoint="+srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// An object the store accepted is stored: a staging file that cannot be
// removed afterwards is a leak to report, not a failed put. Answering an
// error there made the caller treat a stored object as missing (and, for an
// upload, roll back a write that landed).
func TestPutFromTempFile_AStagingFileThatCannotBeRemovedIsNotAFailedPut(t *testing.T) {
	d := fakeStore(t)
	tmp := filepath.Join(t.TempDir(), "upload.tmp")
	if err := os.WriteFile(tmp, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := removeStaging
	removeStaging = func(string) error { return errors.New("read-only file system") }
	defer func() { removeStaging = prev }()
	key, err := d.PutFromTempFile(context.Background(), "k", tmp)
	if err != nil || key != "k" {
		t.Fatalf("PutFromTempFile = %q, %v; want the key and no error — the object is stored", key, err)
	}
}

// A bare 404 is "not found" only where S3 answers a missing object with
// one: HEAD (Stat). On any other operation it names a wrong endpoint or
// path, and reading it as ErrNotFound would report a misconfigured store
// as a missing file.
func TestNotFound_ABare404IsMissingOnlyForAHead(t *testing.T) {
	d := fakeStore(t)
	ctx := context.Background()
	if _, err := d.Stat(ctx, "missing"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Stat over a bare 404 = %v, want ErrNotFound", err)
	}
	if _, err := d.Open(ctx, "missing"); err == nil || errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open over a bare 404 = %v, want a store error, not ErrNotFound", err)
	}
	if err := d.Delete(ctx, "missing"); err == nil {
		t.Error("Delete over a bare 404 succeeded: a wrong endpoint read as an already-deleted object")
	}
	for _, err := range []error{
		func() error { _, err := d.Open(ctx, "missing"); return err }(),
		d.Delete(ctx, "missing"),
	} {
		if err != nil && (strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "AKID")) {
			t.Errorf("an error echoes the DSN's credentials: %v", err)
		}
	}
}
