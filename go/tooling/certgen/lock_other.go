//go:build !unix

package certgen

// lockDir is a best-effort no-op on platforms without flock(2). The
// cross-process serialisation EnsureCerts relies on (Q56-tls-1) is
// unavailable here, so two concurrent invocations on the same directory
// remain racy — a documented limitation: w17ctl's primary targets are
// unix (developer macOS / Linux + Docker). dir is accepted for signature
// parity with the unix implementation.
func lockDir(dir string) (func(), error) {
	return func() {}, nil
}
