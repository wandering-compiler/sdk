//go:build unix

package local_fs

import (
	"errors"
	"syscall"
)

// errEXDEV is the canonical "cross-device link" sentinel.
// Used by [PutFromTempFile] to detect the rename-across-fs
// case + fall back to copy+delete.
var errEXDEV = syscall.EXDEV

// isCrossDeviceErr reports whether err is the syscall.EXDEV
// shape after wrapping. errors.Is handles the common path;
// the secondary string-match catches wrappers that don't
// implement Unwrap (rare but possible from older deps).
func isCrossDeviceErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	return false
}
