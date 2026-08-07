package i18n_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// Register with an empty lang is a no-op — it must not register an
// empty-keyed catalog that T() could accidentally resolve against.
func TestRegister_EmptyLangNoOp(t *testing.T) {
	i18n.Reset()
	t.Cleanup(i18n.Reset)
	i18n.Register("", []byte("anything"))
	// The empty-tag locale resolves to nothing; T falls back to msgid.
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, ""))
	if got := i18n.T(ctx, "bare", nil); got != "bare" {
		t.Errorf("T after empty Register = %q, want bare", got)
	}
}

// RegisterFS walks nested directories and derives the language tag
// from each .po base filename, ignoring non-.po files and dirs.
func TestRegisterFS_NestedWalk(t *testing.T) {
	i18n.Reset()
	t.Cleanup(i18n.Reset)
	csBody, err := i18n.MarshalPO("cs", []i18n.POEntry{{Msgid: "hi", Msgstr: "ahoj"}})
	if err != nil {
		t.Fatalf("MarshalPO: %v", err)
	}
	fsys := fstest.MapFS{
		"i18n/cs.po":          {Data: csBody},
		"i18n/nested/en.po":   {Data: []byte("")},
		"i18n/README.md":      {Data: []byte("not a catalog")},
		"i18n/nested/sub.txt": {Data: []byte("ignored")},
	}
	langs, err := i18n.RegisterFS(fsys)
	if err != nil {
		t.Fatalf("RegisterFS: %v", err)
	}
	has := map[string]bool{}
	for _, l := range langs {
		has[l] = true
	}
	if !has["cs"] || !has["en"] {
		t.Errorf("registered langs = %v, want cs+en", langs)
	}
	if len(langs) != 2 {
		t.Errorf("registered %d langs, want exactly 2 (non-.po ignored): %v", len(langs), langs)
	}
	// Confirm the cs catalog is live.
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, "cs"))
	if got := i18n.T(ctx, "hi", nil); got != "ahoj" {
		t.Errorf("T(cs, hi) = %q, want ahoj", got)
	}
}

// errFS is an fs.FS whose .po files fail on Read, so RegisterFS
// surfaces the read error rather than swallowing it.
type errFS struct{}

func (errFS) Open(name string) (fs.File, error) {
	if name == "." {
		return fstest.MapFS{"broken.po": {Data: []byte("x")}}.Open(name)
	}
	return errFile{name: name}, nil
}

type errFile struct{ name string }

func (f errFile) Stat() (fs.FileInfo, error) { return errFileInfo(f), nil }
func (errFile) Read([]byte) (int, error)     { return 0, errors.New("simulated read failure") }
func (errFile) Close() error                 { return nil }

type errFileInfo struct{ name string }

func (i errFileInfo) Name() string     { return i.name }
func (errFileInfo) Size() int64        { return 1 }
func (errFileInfo) Mode() fs.FileMode  { return 0 }
func (errFileInfo) ModTime() time.Time { return time.Time{} }
func (errFileInfo) IsDir() bool        { return false }
func (errFileInfo) Sys() interface{}   { return nil }

// RegisterFS propagates a ReadFile error encountered mid-walk.
func TestRegisterFS_ReadError(t *testing.T) {
	i18n.Reset()
	t.Cleanup(i18n.Reset)
	if _, err := i18n.RegisterFS(errFS{}); err == nil {
		t.Error("RegisterFS over a failing FS: expected error, got nil")
	}
}
