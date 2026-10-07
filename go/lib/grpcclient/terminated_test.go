package grpcclient

import "testing"

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
