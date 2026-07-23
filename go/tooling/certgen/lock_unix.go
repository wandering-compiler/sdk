//go:build unix

package certgen

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes an exclusive, cross-process advisory lock scoped to dir
// and returns a release closure. It serialises concurrent EnsureCerts
// runs — two `w17ctl init` / `w17ctl certs` invocations pointed at the
// same cert directory — so their check-then-write mint sequences cannot
// interleave into a mismatched cert/key pair (Q56-tls-1).
//
// The lock is an flock(2) on a `.certgen.lock` sentinel inside dir.
// flock is held by the open file description and the kernel drops it
// automatically when the descriptor closes OR the process dies — so a
// crash mid-mint leaves no stale lock to wedge the next run. dir must
// already exist (EnsureCerts mkdir's it first). Distinct open fds
// contend even within one process, so this also serialises concurrent
// in-process callers.
func lockDir(dir string) (func(), error) {
	lockPath := filepath.Join(dir, lockFile)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("certgen: open lock %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("certgen: lock %q: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
