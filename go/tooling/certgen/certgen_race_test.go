package certgen

import (
	"crypto/ecdsa"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Q56-tls-1: concurrent EnsureCerts on one directory must not interleave
// two independent mint sequences into a mismatched cert/key pair.
//
// Before the directory lock, two `w17ctl init` / `w17ctl certs` processes
// that both observed a fresh dir each minted their own CA + leaf, then
// their writeCertPEM / writeKeyPEM calls interleaved (cert from one
// process, key from the other) — leaving a cert that does not match its
// private key, a broken TLS handshake, and (depending on timing) spurious
// "half-present" refusals. With the lock the runs serialise: the first
// writer mints the full PKI, the rest observe a complete set and write
// nothing.
//
// The race is timing-dependent, so the test runs many concurrent trials
// and demands every one produce a self-consistent on-disk pair with no
// errors. Pre-fix this fails reliably across the trial fan; post-fix the
// flock serialisation makes it deterministic.
func TestEnsureCerts_ConcurrentMint_ConsistentPair(t *testing.T) {
	const trials, racers = 40, 8
	for trial := 0; trial < trials; trial++ {
		dir := t.TempDir()
		var wg sync.WaitGroup
		errs := make([]error, racers)
		wg.Add(racers)
		for i := 0; i < racers; i++ {
			go func(i int) {
				defer wg.Done()
				_, errs[i] = EnsureCerts(dir, []string{"localhost"})
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("trial %d racer %d: EnsureCerts errored under concurrency: %v", trial, i, err)
			}
		}
		if err := verifyOnDiskPair(dir); err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
	}
}

// verifyOnDiskPair re-loads the persisted CA + leaf and asserts they form
// a coherent chain: each cert's public key matches its own private key,
// and the leaf is genuinely signed by the on-disk CA. A mismatched pair
// (the Q56-tls-1 race) trips exactly one of these.
func verifyOnDiskPair(dir string) error {
	caCert, caKey, err := loadCert(filepath.Join(dir, caFile), filepath.Join(dir, caKeyFile))
	if err != nil {
		return fmt.Errorf("load CA: %w", err)
	}
	caPub, ok := caCert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("ca.crt public key is %T, want *ecdsa.PublicKey", caCert.PublicKey)
	}
	if !caPub.Equal(&caKey.PublicKey) {
		return fmt.Errorf("CA mismatch: ca.crt public key does not match ca.key")
	}

	leafCert, leafKey, err := loadCert(filepath.Join(dir, serverCertFile), filepath.Join(dir, serverKeyFile))
	if err != nil {
		return fmt.Errorf("load leaf: %w", err)
	}
	leafPub, ok := leafCert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("server.crt public key is %T, want *ecdsa.PublicKey", leafCert.PublicKey)
	}
	if !leafPub.Equal(&leafKey.PublicKey) {
		return fmt.Errorf("leaf mismatch: server.crt public key does not match server.key")
	}
	if err := leafCert.CheckSignatureFrom(caCert); err != nil {
		return fmt.Errorf("leaf not signed by on-disk CA (CA cert/key interleaved): %w", err)
	}
	return nil
}
