//go:build unix

package local_fs

import (
	"fmt"
	"syscall"
	"testing"
)

// TestIsCrossDeviceErr pins the EXDEV detection used by PutFromTempFile
// to choose the copy+delete fallback: nil is not cross-device, a wrapped
// syscall.EXDEV is, and an unrelated error is not.
func TestIsCrossDeviceErr(t *testing.T) {
	if isCrossDeviceErr(nil) {
		t.Error("isCrossDeviceErr(nil) = true, want false")
	}
	if !isCrossDeviceErr(syscall.EXDEV) {
		t.Error("isCrossDeviceErr(EXDEV) = false, want true")
	}
	if !isCrossDeviceErr(fmt.Errorf("rename: %w", syscall.EXDEV)) {
		t.Error("isCrossDeviceErr(wrapped EXDEV) = false, want true")
	}
	if isCrossDeviceErr(syscall.ENOENT) {
		t.Error("isCrossDeviceErr(ENOENT) = true, want false")
	}
}
