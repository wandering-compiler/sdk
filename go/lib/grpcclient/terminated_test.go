package grpcclient

import (
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
