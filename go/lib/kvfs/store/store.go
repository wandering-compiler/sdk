// Package store opens an upload connection's file store from ONE URL, so the
// same binary writes to a directory on a laptop and to a bucket in
// production, chosen by the environment rather than by the schema:
//
//	file:///var/lib/w17/uploads/media     a directory (local_fs)
//	/var/lib/w17/uploads/media            the same, as a plain absolute path
//	s3://<key-id>:<key>@media?endpoint=…   a bucket (kvfs/s3), credentials optional
//
// The URL can carry a secret, so no error here repeats it: an S3 store is
// named by its bucket, a directory by its path.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	localfs "github.com/wandering-compiler/sdk/go/lib/kvfs/local_fs"
	kvs3 "github.com/wandering-compiler/sdk/go/lib/kvfs/s3"
)

// Open opens the store raw names. ensure is the WRITER's flag (the gateway):
// it creates a missing directory or bucket, the way a fresh dev stack needs.
// A reader passes false and is refused a store that is not there — a reader
// that created its own root would turn a typo into an empty store whose every
// read says "not found".
func Open(ctx context.Context, raw string, ensure bool) (kvfs.Driver, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return nil, errors.New("kvfs store: no URL (file:///<dir>, an absolute path, or s3://<bucket>)")
	case strings.HasPrefix(raw, "s3://"):
		d, err := kvs3.New(ctx, raw)
		if err != nil {
			return nil, err
		}
		if ensure {
			if err := d.EnsureBucket(ctx); err != nil {
				return nil, err
			}
		}
		return d, nil
	case strings.HasPrefix(raw, "file://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host != "" || u.Path == "" {
			return nil, fmt.Errorf("kvfs store: %q is not a file URL — write file:///<absolute path> (three slashes)", raw)
		}
		return openDir(u.Path, ensure)
	case strings.HasPrefix(raw, "/"):
		return openDir(raw, ensure)
	}
	scheme, _, _ := strings.Cut(raw, "://")
	if strings.Contains(raw, "://") {
		return nil, fmt.Errorf("kvfs store: scheme %q has no driver — file:// or s3://", scheme)
	}
	return nil, errors.New("kvfs store: a relative path is not a store — write an absolute path or file:///<path>")
}

func openDir(dir string, ensure bool) (kvfs.Driver, error) {
	if ensure {
		if err := localfs.EnsureRoot(dir); err != nil {
			return nil, fmt.Errorf("kvfs store: ensure %s: %w", dir, err)
		}
		return localfs.New(dir), nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("kvfs store: %s is not readable: %w (the writer creates it; a reader that cannot see it would answer not-found for every stored object)", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("kvfs store: %s is not a directory", dir)
	}
	return localfs.New(dir), nil
}
