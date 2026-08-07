package grpcclient_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// TestDial_NilLookupDefaults — a nil lookup defaults to os.Getenv. With the
// internal-TLS switch off (the default) Dial builds a lazy plain-h2c client
// without error (grpc.NewClient does not dial eagerly).
func TestDial_NilLookupDefaults(t *testing.T) {
	conn, err := grpcclient.Dial("localhost:1", nil)
	if err != nil {
		t.Fatalf("Dial with nil lookup: %v", err)
	}
	if conn == nil {
		t.Fatal("nil conn")
	}
	_ = conn.Close()
}
