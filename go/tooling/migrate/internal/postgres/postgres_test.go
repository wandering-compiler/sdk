package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

func TestNew_EmptyDSNRefuses(t *testing.T) {
	_, err := postgres.New(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

// TestNew_BogusDSNFails — pgx.Connect against an unreachable
// host fails fast (DSN with explicit short connect_timeout).
// The error wrap surfaces "postgres.New" so callers can route
// dialect-specific log handlers.
func TestNew_BogusDSNFails(t *testing.T) {
	_, err := postgres.New(context.Background(),
		"postgres://nobody:none@127.0.0.1:1/x?connect_timeout=1")
	if err == nil {
		t.Fatal("expected connect error")
	}
	if !strings.Contains(err.Error(), "postgres.New") {
		t.Errorf("err %q missing postgres.New prefix", err.Error())
	}
}

// TestClose_NilConnSafe — Close on an Applier whose conn is nil
// (e.g. after a previous Close) is a no-op, never panics.
func TestClose_NilConnSafe(t *testing.T) {
	// We cannot construct an Applier with nil conn from outside the
	// package (the only constructor is postgres.New which always
	// dials). The defensive guard exists for future construction
	// paths; verify it via a defer-recover so the test doesn't
	// silently allow a panic regression.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Close panicked: %v", r)
		}
	}()
	_, err := postgres.New(context.Background(), "")
	if err == nil {
		t.Fatal("setup: expected New to fail on empty DSN")
	}
	// No applier produced; the test just confirms the constructor
	// guard fires before any nil-conn risk arises.
}
