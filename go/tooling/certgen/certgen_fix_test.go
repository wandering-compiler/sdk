package certgen

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R-certgen-1: no-clobber invariant. If exactly ONE of ca.crt /
// ca.key is present, EnsureCerts must refuse rather than mint a fresh
// CA over the surviving half (which would invalidate distributed
// trust / any leaf the old key signed).

func TestEnsureCA_HalfPresent_KeyMissing_Refused(t *testing.T) {
	dir := t.TempDir()
	// Seed a full PKI, then remove only the CA key.
	if _, err := EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	certBefore, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, caKeyFile)); err != nil {
		t.Fatal(err)
	}
	_, err = EnsureCerts(dir, []string{"localhost"})
	if err == nil {
		t.Fatal("expected refusal when only ca.crt survives")
	}
	if !strings.Contains(err.Error(), "half-present") {
		t.Errorf("error should explain the half-present refusal; got: %v", err)
	}
	// The surviving cert must NOT have been overwritten.
	certAfter, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(certBefore) != string(certAfter) {
		t.Error("surviving ca.crt was clobbered despite the refusal")
	}
}

func TestEnsureCA_HalfPresent_CertMissing_Refused(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	keyBefore, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, caFile)); err != nil {
		t.Fatal(err)
	}
	_, err = EnsureCerts(dir, []string{"localhost"})
	if err == nil {
		t.Fatal("expected refusal when only ca.key survives")
	}
	if !strings.Contains(err.Error(), "half-present") {
		t.Errorf("error should explain the half-present refusal; got: %v", err)
	}
	keyAfter, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(keyBefore) != string(keyAfter) {
		t.Error("surviving ca.key was clobbered despite the refusal")
	}
}

// Neither half present (fresh dir) must still mint cleanly — the
// refusal only fires on the half-present case.
func TestEnsureCA_NeitherPresent_Mints(t *testing.T) {
	dir := t.TempDir()
	written, err := EnsureCerts(dir, []string{"localhost"})
	if err != nil {
		t.Fatalf("fresh mint should succeed: %v", err)
	}
	if len(written) != 4 {
		t.Errorf("fresh dir should write 4 files, wrote %d", len(written))
	}
}

// R-certgen-2: splitSANs must de-dup IPs (it already de-dups DNS).
// A caller-supplied loopback must not produce a duplicate entry when
// the unconditional loopback pair is appended.

func TestSplitSANs_DedupsLoopbackIP(t *testing.T) {
	_, ips := splitSANs([]string{"127.0.0.1", "::1", "10.0.0.5", "10.0.0.5"})
	seen := map[string]int{}
	for _, ip := range ips {
		seen[ip.String()]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("IP %s appears %d times, want exactly 1", k, n)
		}
	}
	// Sanity: loopback + the real IP are all present.
	for _, want := range []string{"127.0.0.1", "::1", "10.0.0.5"} {
		if seen[want] == 0 {
			t.Errorf("expected IP %s in SANs", want)
		}
	}
}

func TestSplitSANs_LoopbackAlwaysPresent(t *testing.T) {
	_, ips := splitSANs(nil)
	var hasV4, hasV6 bool
	for _, ip := range ips {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			hasV4 = true
		}
		if ip.Equal(net.IPv6loopback) {
			hasV6 = true
		}
	}
	if !hasV4 || !hasV6 {
		t.Errorf("loopback pair must always be present; v4=%v v6=%v", hasV4, hasV6)
	}
	if len(ips) != 2 {
		t.Errorf("empty sans should yield exactly the 2 loopback IPs, got %d", len(ips))
	}
}

// Q17-certgen-1: the no-clobber invariant must also hold for the LEAF
// (server.crt/server.key), mirroring the CA guard. If exactly one half
// survives, EnsureCerts must refuse rather than overwrite the survivor.
func TestEnsureLeaf_HalfPresent_KeyMissing_Refused(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	certBefore, err := os.ReadFile(filepath.Join(dir, serverCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, serverKeyFile)); err != nil {
		t.Fatal(err)
	}
	_, err = EnsureCerts(dir, []string{"localhost"})
	if err == nil {
		t.Fatal("expected refusal when only server.crt survives")
	}
	if !strings.Contains(err.Error(), "half-present") {
		t.Errorf("error should explain the half-present refusal; got: %v", err)
	}
	certAfter, err := os.ReadFile(filepath.Join(dir, serverCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(certBefore) != string(certAfter) {
		t.Error("surviving server.crt was clobbered despite the refusal")
	}
}

func TestEnsureLeaf_HalfPresent_CertMissing_Refused(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	keyBefore, err := os.ReadFile(filepath.Join(dir, serverKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, serverCertFile)); err != nil {
		t.Fatal(err)
	}
	_, err = EnsureCerts(dir, []string{"localhost"})
	if err == nil {
		t.Fatal("expected refusal when only server.key survives")
	}
	if !strings.Contains(err.Error(), "half-present") {
		t.Errorf("error should explain the half-present refusal; got: %v", err)
	}
	keyAfter, err := os.ReadFile(filepath.Join(dir, serverKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(keyBefore) != string(keyAfter) {
		t.Error("surviving server.key was clobbered despite the refusal")
	}
}
