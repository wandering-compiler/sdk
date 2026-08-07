package nats_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
)

func TestNewSnapshotter_EmptyDSNRefuses(t *testing.T) {
	_, err := nats.NewSnapshotter("")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

// TestNewSnapshotter_OK — a valid DSN builds a lazy-connect
// Snapshotter (no TCP connection until Dump/Restore).
func TestNewSnapshotter_OK(t *testing.T) {
	s, err := nats.NewSnapshotter("nats://127.0.0.1:4222")
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Snapshotter")
	}
}
