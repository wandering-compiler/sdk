package redis_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, context.Canceled }

// Dump's DUMP-error arm (non-Nil): a hash key — which miniredis's DUMP
// rejects with WRONGTYPE — is found by SCAN but fails the per-key DUMP,
// surfacing the error rather than being skipped.
func TestRedis_DumpWrongType(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.HSet("ahash", "f", "v")
	if err := snapFor(t, mr).Dump(context.Background(), &bytes.Buffer{}); err == nil {
		t.Fatal("want a DUMP error on a non-dumpable key type")
	}
}

// Dump's encode-error arm: a writer that always fails makes the per-record
// gob encode return an error after SCAN/DUMP/PTTL succeed.
func TestRedis_DumpEncodeError(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.Set("k", "v")
	if err := snapFor(t, mr).Dump(context.Background(), errWriter{}); err == nil {
		t.Fatal("want an encode error from a failing writer")
	}
}

// Wipe's FlushDB error arm, driven deterministically with a cancelled
// context (FlushDB returns context.Canceled rather than completing).
func TestRedis_WipeError(t *testing.T) {
	a, _ := liveApplier(t)
	if err := a.Wipe(cancelledCtx()); err == nil {
		t.Fatal("want Wipe error under a cancelled context")
	}
}

// Dump's SCAN error arm under a cancelled context.
func TestRedis_DumpScanError(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.Set("k", "v")
	s := snapFor(t, mr)
	if err := s.Dump(cancelledCtx(), &bytes.Buffer{}); err == nil {
		t.Fatal("want Dump SCAN error under a cancelled context")
	}
}

// Restore's RESTORE error arm: a real dump stream replayed under a
// cancelled context fails at RestoreReplace.
func TestRedis_RestoreError(t *testing.T) {
	src := miniredis.RunT(t)
	src.Set("k", "v")
	var buf bytes.Buffer
	if err := snapFor(t, src).Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if err := snapFor(t, miniredis.RunT(t)).Restore(cancelledCtx(), &buf); err == nil {
		t.Fatal("want Restore error under a cancelled context")
	}
}

// run() skips blank lines between commands (the `line == ""` continue arm).
func TestRedis_ApplyBlankLinesBetweenCommands(t *testing.T) {
	a, mr := liveApplier(t)
	if err := a.Apply(context.Background(), &applyfetchpb.Migration{
		UpSql: "SET a 1\n\n\nSET b 2",
	}); err != nil {
		t.Fatalf("Apply with blank lines: %v", err)
	}
	if v, _ := mr.Get("a"); v != "1" {
		t.Errorf("a = %q, want 1", v)
	}
	if v, _ := mr.Get("b"); v != "2" {
		t.Errorf("b = %q, want 2", v)
	}
}
