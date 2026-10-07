package grpcserver

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// Under "terminated" a server needs no certificate: TLS ends in front of it.
func TestTLSServerOption_TerminatedListensPlain(t *testing.T) {
	opt, on, err := TLSServerOption(func(k string) string {
		if k == grpcclient.EnvInternalTLS {
			return "terminated"
		}
		return ""
	})
	if err != nil || on || opt != nil {
		t.Fatalf("terminated: opt=%v on=%v err=%v — want a plaintext listener and no error", opt, on, err)
	}
}
