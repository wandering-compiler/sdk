package grpcclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "terminated": the platform ends TLS in front of every service, so a client
// dials TLS and a server listens in plaintext.
func TestInternalTLS_Terminated(t *testing.T) {
	for v, want := range map[string][2]bool{ // {server listens TLS, client dials TLS}
		"":           {false, false},
		"off":        {false, false},
		"on":         {true, true},
		"terminated": {false, true},
		"Terminated": {false, true},
	} {
		if got := [2]bool{InternalTLSEnabled(v), InternalTLSDial(v)}; got != want {
			t.Errorf("%q: server/client TLS = %v, want %v", v, got, want)
		}
	}
	// The dial option is a TLS one under "terminated", with no stack CA:
	// the platform's certificate is verified against the system roots.
	opt, err := TLSDialOption(func(k string) string {
		if k == EnvInternalTLS {
			return "terminated"
		}
		return ""
	})
	if err != nil || opt == nil {
		t.Fatalf("terminated dial option: %v", err)
	}
	plain, _ := TLSDialOption(func(string) string { return "" })
	if opt == plain {
		t.Error("terminated produced the plaintext dial option")
	}
}

// A switch value outside the recognised spellings is refused, not read as
// off: a typo must not quietly run the whole mesh in plaintext.
func TestCheckInternalTLS(t *testing.T) {
	for _, v := range []string{"", "off", "OFF", "false", "0", "no", " on ", "true", "1", "yes", "terminated", "Terminated"} {
		if err := CheckInternalTLS(v); err != nil {
			t.Errorf("%q: %v, want accepted", v, err)
		}
	}
	for _, v := range []string{"enabled", "required", "tls", "maybe", "o n", "on​"} {
		err := CheckInternalTLS(v)
		if err == nil {
			t.Errorf("%q accepted, want refused", v)
			continue
		}
		for _, want := range []string{EnvInternalTLS, "on", "terminated", "off"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: error %q does not mention %q", v, err, want)
			}
		}
	}
}

func TestTLSDialOption_UnrecognisedSwitchIsRefused(t *testing.T) {
	_, err := TLSDialOption(func(k string) string {
		if k == EnvInternalTLS {
			return "enabled"
		}
		return ""
	})
	if err == nil {
		t.Fatal("TLSDialOption accepted W17_INTERNAL_TLS=enabled (and dialed plaintext)")
	}
	if _, err := Dial("localhost:1", func(k string) string {
		if k == EnvInternalTLS {
			return "required"
		}
		return ""
	}); err == nil {
		t.Fatal("Dial accepted W17_INTERNAL_TLS=required")
	}
}

// Under "terminated" the server presents the platform's certificate, so the
// client verifies it against the system roots alone. A private CA or client
// pair left over from a TLS-on stack is ignored, with a warning, instead of
// replacing the system roots and failing every handshake with the platform.
func TestClientTLSConfig_TerminatedUsesSystemRootsOnly(t *testing.T) {
	var warned []string
	prev := warnf
	warnf = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { warnf = prev })

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("not even a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		EnvInternalTLS:     "terminated",
		EnvInternalTLSCA:   caPath,
		EnvInternalTLSCert: filepath.Join(dir, "missing.crt"),
		EnvInternalTLSKey:  filepath.Join(dir, "missing.key"),
	}
	cfg, err := ClientTLSConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("terminated with a private CA set: %v (the CA must be ignored, not loaded)", err)
	}
	if cfg == nil {
		t.Fatal("terminated must dial TLS")
	}
	if cfg.RootCAs != nil {
		t.Error("terminated replaced the system roots with the private CA")
	}
	if len(cfg.Certificates) != 0 {
		t.Error("terminated presents a client certificate")
	}
	if len(warned) != 1 || !strings.Contains(warned[0], EnvInternalTLSCA) || !strings.Contains(warned[0], EnvInternalTLSCert) {
		t.Errorf("want one warning naming the ignored variables, got %q", warned)
	}
	if _, err := TLSDialOption(func(k string) string { return env[k] }); err != nil {
		t.Errorf("TLSDialOption under terminated: %v", err)
	}

	// Nothing set beyond the switch: no warning.
	warned = nil
	if _, err := ClientTLSConfig(func(k string) string {
		if k == EnvInternalTLS {
			return "terminated"
		}
		return ""
	}); err != nil || len(warned) != 0 {
		t.Errorf("bare terminated: err=%v warnings=%q", err, warned)
	}
}

func TestClientTLSConfig_OffIsNil(t *testing.T) {
	for _, lookup := range []LookupFunc{nil, func(string) string { return "" }} {
		cfg, err := ClientTLSConfig(lookup)
		if err != nil || cfg != nil {
			t.Errorf("switch off: (%v, %v), want (nil, nil)", cfg, err)
		}
	}
}
