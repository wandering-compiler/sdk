package postgres_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

func TestNewSnapshotter_EmptyDSNRefuses(t *testing.T) {
	_, err := postgres.NewSnapshotter("")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

// TestNewSnapshotter_OK — a non-empty DSN constructs without dialing
// (pg_dump / psql connect lazily per invocation), so even a bogus host
// yields a usable Snapshotter; the connection error only appears when
// Dump/Restore runs the client binary.
func TestNewSnapshotter_OK(t *testing.T) {
	s, err := postgres.NewSnapshotter("postgres://nobody:none@127.0.0.1:1/x")
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Snapshotter")
	}
}
