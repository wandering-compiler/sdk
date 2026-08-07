package s3_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
)

func TestNewSnapshotter_Guards(t *testing.T) {
	cases := map[string]string{ // dsn → expected error substring
		"":          "dsn is empty",
		"redis://x": "expected s3:// scheme",
		"s3://":     "missing bucket",
	}
	for dsn, want := range cases {
		t.Run(dsn, func(t *testing.T) {
			_, err := s3.NewSnapshotter(dsn)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("dsn %q: want error containing %q, got %v", dsn, want, err)
			}
		})
	}
}

// TestNewSnapshotter_OK — a valid DSN builds a lazy-connect
// Snapshotter (AWS client constructed on first Dump/Restore).
func TestNewSnapshotter_OK(t *testing.T) {
	s, err := s3.NewSnapshotter("s3://my-bucket?endpoint=http://127.0.0.1:9000&region=us-east-1")
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Snapshotter")
	}
}
