//go:build !unix

package local_fs

import "errors"

// Non-unix builds (Windows, JS) don't have a syscall.EXDEV;
// the cross-device path is unreachable on these platforms
// (Windows fs APIs return different error codes; the gateway
// is unix-only in practice). The variables exist to satisfy
// the call sites in local_fs.go without conditional code
// paths sprinkled through the main file.

var errEXDEV = errors.New("local_fs: cross-device link not applicable on this platform")

func isCrossDeviceErr(_ error) bool { return false }
