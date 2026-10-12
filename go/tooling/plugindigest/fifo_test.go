//go:build unix

package plugindigest

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A named pipe is refused like a symlink: hashing it would block on a
// reader, or depend on whatever wrote into it.
func TestOf_RefusesANamedPipe(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"plugin.yaml": "name: p\n"})
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := Of(dir); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("Of = %v, want a refusal of the named pipe", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Of blocked reading the named pipe instead of refusing it")
	}
}
