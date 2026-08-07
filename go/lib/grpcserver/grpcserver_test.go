package grpcserver

import (
	"path/filepath"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/certgen"
)

// certDir mints a dev PKI in a temp dir and returns the cert/key/CA paths.
func certDir(t *testing.T) (certPath, keyPath, caPath string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := certgen.EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatalf("seed certs: %v", err)
	}
	// EnsureCerts writes the conventional dev-PKI filenames into dir (the same
	// set the w17ctl `certs` command documents). The certgen names are package-
	// private; this test pins the contract by literal.
	return filepath.Join(dir, "server.crt"),
		filepath.Join(dir, "server.key"),
		filepath.Join(dir, "ca.crt")
}

// Switch off (the default) → no TLS option. A nil-ish lookup that
// returns "" for everything means the stack-wide switch is off.
func TestSwitchOffNoTLS(t *testing.T) {
	opt, enabled, err := TLSServerOption(func(string) string { return "" })
	if err != nil || enabled || opt != nil {
		t.Fatalf("switch off must yield no TLS: opt=%v enabled=%v err=%v", opt, enabled, err)
	}
}

// Explicitly off is the same as unset — no TLS.
func TestExplicitOff(t *testing.T) {
	env := map[string]string{"W17_INTERNAL_TLS": "off"}
	opt, enabled, err := TLSServerOption(func(k string) string { return env[k] })
	if err != nil || enabled || opt != nil {
		t.Fatalf("W17_INTERNAL_TLS=off must disable TLS: opt=%v enabled=%v err=%v", opt, enabled, err)
	}
}

func TestSwitchOnRequiresCertKey(t *testing.T) {
	// Switch on but no server leaf cert/key → fail fast at bootstrap.
	env := map[string]string{"W17_INTERNAL_TLS": "on"}
	_, enabled, err := TLSServerOption(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("switch-on TLS without cert/key must error at bootstrap")
	}
	if enabled {
		t.Fatal("misconfigured TLS must not report enabled")
	}
}

func TestServerAuthTLSEnabled(t *testing.T) {
	cert, key, _ := certDir(t)
	env := map[string]string{
		"W17_INTERNAL_TLS":      "on",
		"W17_INTERNAL_TLS_CERT": cert,
		"W17_INTERNAL_TLS_KEY":  key,
	}
	opt, enabled, err := TLSServerOption(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("valid cert/key: %v", err)
	}
	if !enabled || opt == nil {
		t.Fatalf("expected TLS enabled with a server option, got enabled=%v opt=%v", enabled, opt)
	}
}

func TestMutualTLSWithClientCA(t *testing.T) {
	cert, key, ca := certDir(t)
	env := map[string]string{
		"W17_INTERNAL_TLS":           "on",
		"W17_INTERNAL_TLS_CERT":      cert,
		"W17_INTERNAL_TLS_KEY":       key,
		"W17_INTERNAL_TLS_CLIENT_CA": ca,
	}
	opt, enabled, err := TLSServerOption(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("mTLS config: %v", err)
	}
	if !enabled || opt == nil {
		t.Fatal("mTLS must enable TLS")
	}
}

func TestBadClientCAErrors(t *testing.T) {
	cert, key, _ := certDir(t)
	env := map[string]string{
		"W17_INTERNAL_TLS":           "on",
		"W17_INTERNAL_TLS_CERT":      cert,
		"W17_INTERNAL_TLS_KEY":       key,
		"W17_INTERNAL_TLS_CLIENT_CA": filepath.Join(t.TempDir(), "nope.crt"),
	}
	if _, enabled, err := TLSServerOption(func(k string) string { return env[k] }); err == nil || enabled {
		t.Fatal("unreadable client CA must error")
	}
}
