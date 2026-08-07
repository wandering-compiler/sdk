package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	memdriver "github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// stagingDriver records the tmpPath ProcessFilePart handed it and
// advertises a staging directory of its own (the kvfs.StagingDirProvider
// contract a local-filesystem driver implements so the publish stays a
// same-filesystem rename).
type stagingDriver struct {
	*memdriver.Driver
	dir     string
	err     error
	sawTmp  string
	staged  bool
	putSeen bool
}

func (s *stagingDriver) StagingDir() (string, error) {
	s.staged = true
	if s.err != nil {
		return "", s.err
	}
	return s.dir, nil
}

func (s *stagingDriver) PutFromTempFile(ctx context.Context, key string, tmpPath string) (string, error) {
	s.sawTmp = tmpPath
	s.putSeen = true
	return s.Driver.PutFromTempFile(ctx, key, tmpPath)
}

// TestProcessFilePart_StagesInTheDriversOwnDir pins the fix for the
// second half of the EXDEV finding: with no explicit TmpDir — which is
// what EVERY generated gateway emits — the handler must ask the driver
// where to stage, so the file lands on the bucket's own filesystem and
// PutFromTempFile is a plain rename rather than the copy fallback.
func TestProcessFilePart_StagesInTheDriversOwnDir(t *testing.T) {
	stage := t.TempDir()
	d := &stagingDriver{Driver: memdriver.New(), dir: stage}
	req := multipartReq(t)

	if _, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	}); err != nil {
		t.Fatalf("ProcessFilePart: %v", err)
	}
	if !d.staged {
		t.Fatal("ProcessFilePart never asked the driver for a staging dir")
	}
	if got := filepath.Dir(d.sawTmp); got != stage {
		t.Errorf("staged upload in %q, want the driver's staging dir %q", got, stage)
	}
}

// TestProcessFilePart_ExplicitTmpDirWins pins that an explicit TmpDir
// still overrides the driver's suggestion — the config field keeps its
// documented meaning for hand-written callers.
func TestProcessFilePart_ExplicitTmpDirWins(t *testing.T) {
	stage, explicit := t.TempDir(), t.TempDir()
	d := &stagingDriver{Driver: memdriver.New(), dir: stage}
	req := multipartReq(t)

	if _, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		TmpDir:            explicit,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	}); err != nil {
		t.Fatalf("ProcessFilePart: %v", err)
	}
	if d.staged {
		t.Error("ProcessFilePart consulted the driver even though TmpDir was set")
	}
	if got := filepath.Dir(d.sawTmp); got != explicit {
		t.Errorf("staged upload in %q, want the explicit TmpDir %q", got, explicit)
	}
}

// TestProcessFilePart_StagingDirError pins the ERROR path: a driver that
// cannot produce a staging dir (unwritable bucket root) fails the upload
// loudly instead of silently staging somewhere that will need the
// non-atomic fallback.
func TestProcessFilePart_StagingDirError(t *testing.T) {
	boom := errors.New("bucket root is read-only")
	d := &stagingDriver{Driver: memdriver.New(), err: boom}
	req := multipartReq(t)

	_, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            d,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ProcessFilePart err = %v, want the driver's staging error", err)
	}
	if d.putSeen {
		t.Error("ProcessFilePart put the object despite failing to stage")
	}
}

// TestProcessFilePart_NoStagingProviderKeepsOSTempDir pins that drivers
// with no local filesystem (S3, the memory driver) are unaffected: they
// advertise nothing and the handler keeps using os.TempDir().
func TestProcessFilePart_NoStagingProviderKeepsOSTempDir(t *testing.T) {
	var d kvfs.Driver = memdriver.New()
	if _, ok := d.(kvfs.StagingDirProvider); ok {
		t.Fatal("memory driver should not advertise a staging dir")
	}
	rec := &tmpPathRecorder{Driver: memdriver.New()}
	req := multipartReq(t)
	if _, err := restgw.ProcessFilePart(context.Background(), req, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            rec,
		BucketPath:        "/x",
		ExpectedObjects:   1,
		MaxPerBucket:      1,
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	}); err != nil {
		t.Fatalf("ProcessFilePart: %v", err)
	}
	if got, want := filepath.Dir(rec.sawTmp), os.TempDir(); got != want {
		t.Errorf("staged upload in %q, want os.TempDir() %q", got, want)
	}
}

type tmpPathRecorder struct {
	*memdriver.Driver
	sawTmp string
}

func (r *tmpPathRecorder) PutFromTempFile(ctx context.Context, key string, tmpPath string) (string, error) {
	r.sawTmp = tmpPath
	return r.Driver.PutFromTempFile(ctx, key, tmpPath)
}

func multipartReq(t *testing.T) *http.Request {
	t.Helper()
	body, contentType := buildMultipart(t, map[string]string{"file": "doc.pdf=payload-bytes"})
	req := httptest.NewRequest(http.MethodPost, "/u", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	return req
}
