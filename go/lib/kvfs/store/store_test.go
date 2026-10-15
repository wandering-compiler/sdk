package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	localfs "github.com/wandering-compiler/sdk/go/lib/kvfs/local_fs"
	kvs3 "github.com/wandering-compiler/sdk/go/lib/kvfs/s3"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/store"
)

func TestOpen_PicksTheDriverByURL(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "media")
	for _, raw := range []string{"file://" + dir, dir} {
		d, err := store.Open(ctx, raw, true)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if _, ok := d.(*localfs.Driver); !ok {
			t.Errorf("%s: driver %T, want local_fs", raw, d)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the writer did not create the directory: %v", err)
	}
	d, err := store.Open(ctx, "s3://k:v@media?endpoint=http://127.0.0.1:1", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*kvs3.Driver); !ok {
		t.Errorf("s3 URL: driver %T, want kvfs/s3", d)
	}
}

func TestOpen_Refuses(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "nope")
	for _, tc := range []struct{ raw, want string }{
		{"", "no URL"},
		{"uploads/media", "relative path"},
		{"file://host/media", "three slashes"},
		{"gs://media", `scheme "gs" has no driver`},
		{missing, "not readable"},
	} {
		_, err := store.Open(ctx, tc.raw, false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.raw, err, tc.want)
		}
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("a reader created the directory it was refused")
	}
	// The URL may carry a secret; a refusal of it must not.
	_, err := store.Open(ctx, "s3://id:topsecret@media?path_style=maybe", false)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("an S3 refusal echoed the key: %v", err)
	}
}
