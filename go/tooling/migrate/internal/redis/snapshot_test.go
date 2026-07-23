package redis_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
)

func TestNewSnapshotter_EmptyDSNRefuses(t *testing.T) {
	_, err := redis.NewSnapshotter("")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

// TestNewSnapshotter_OK — a valid DSN parses + builds the lazy client
// without dialing (go-redis connects on first command).
func TestNewSnapshotter_OK(t *testing.T) {
	s, err := redis.NewSnapshotter("redis://127.0.0.1:6379/0")
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Snapshotter")
	}
}
