package principal_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// DeviceID reads exactly the gateway's stamp: one non-empty value. Absent,
// empty or doubled values are "no device" — two values cannot both be the
// device the proof was checked against.
func TestDeviceID_ReadsTheStamp(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want string
		ok   bool
	}{
		{"no metadata", context.Background(), "", false},
		{"no device", metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-w17-user", "env")), "", false},
		{"empty", metadata.NewIncomingContext(context.Background(), metadata.Pairs(principal.DeviceMetadataKey, "")), "", false},
		{"two values", metadata.NewIncomingContext(context.Background(), metadata.Pairs(principal.DeviceMetadataKey, "a", principal.DeviceMetadataKey, "b")), "", false},
		{"stamped", metadata.NewIncomingContext(context.Background(), metadata.Pairs(principal.DeviceMetadataKey, "jkt-1")), "jkt-1", true},
		{"outgoing only", metadata.NewOutgoingContext(context.Background(), metadata.Pairs(principal.DeviceMetadataKey, "jkt-1")), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := principal.DeviceID(tc.ctx)
			if got != tc.want || ok != tc.ok {
				t.Errorf("DeviceID = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A client's x-w17-device never survives the gateway: the sanitizer strips
// it from the incoming copy, both sides of the gateway context carry only
// the gateway's own stamp, and an unverified request (SetDeviceID with "")
// carries none at all.
func TestDeviceID_ClientCannotInject(t *testing.T) {
	forged := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		principal.DeviceMetadataKey, "attacker-device",
		"X-W17-Device", "attacker-device-2",
		"traceparent", "00-trace-span-01",
	))
	if !principal.IsGatewayOwnedKey("X-W17-Device") || !principal.IsEdgeReservedKey("Grpc-Metadata-X-W17-Device") {
		t.Fatal("x-w17-device must be a gateway-owned key on every edge spelling")
	}

	md := principal.SanitizeGatewayMD(forged)
	if got := md.Get(principal.DeviceMetadataKey); len(got) != 0 {
		t.Fatalf("sanitized metadata kept the client's device %v", got)
	}
	unverified := principal.WithGatewayMD(forged, md.Copy())
	if dev, ok := principal.DeviceID(unverified); ok {
		t.Errorf("unverified request reads device %q from client input", dev)
	}

	principal.SetDeviceID(md, "verified-jkt")
	ctx := principal.WithGatewayMD(forged, md)
	if dev, ok := principal.DeviceID(ctx); !ok || dev != "verified-jkt" {
		t.Errorf("DeviceID = (%q, %v), want the gateway's verified-jkt", dev, ok)
	}
	out, _ := metadata.FromOutgoingContext(ctx)
	if got := out.Get(principal.DeviceMetadataKey); len(got) != 1 || got[0] != "verified-jkt" {
		t.Errorf("outgoing device = %v, want [verified-jkt]", got)
	}

	// An empty id removes a stamp instead of writing an empty value.
	principal.SetDeviceID(md, "")
	if got := md.Get(principal.DeviceMetadataKey); len(got) != 0 {
		t.Errorf("SetDeviceID(\"\") left %v", got)
	}
	principal.SetDeviceID(nil, "x") // no panic on nil
}

// The device id is relayed like the rest of the verified principal, and the
// incoming principal wins: a stale outgoing device (a context reused across
// requests) is dropped when the verified caller carries none.
func TestForwardToOutgoing_RelaysDevice(t *testing.T) {
	ctx := principal.ForwardToOutgoing(metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-w17-user", "env", principal.DeviceMetadataKey, "jkt-1",
	)))
	out, _ := metadata.FromOutgoingContext(ctx)
	if got := out.Get(principal.DeviceMetadataKey); len(got) != 1 || got[0] != "jkt-1" {
		t.Fatalf("outgoing device = %v, want [jkt-1]", got)
	}

	stale := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(principal.DeviceMetadataKey, "previous-callers-device"))
	stale = metadata.NewIncomingContext(stale, metadata.Pairs("x-w17-user", "bearer-caller"))
	out, _ = metadata.FromOutgoingContext(principal.ForwardToOutgoing(stale))
	if got := out.Get(principal.DeviceMetadataKey); len(got) != 0 {
		t.Errorf("a stale outgoing device rode along with another caller's principal: %v", got)
	}

	// Idempotent: a second forward does not duplicate the value.
	twice := principal.ForwardToOutgoing(principal.ForwardToOutgoing(metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-w17-user", "env", principal.DeviceMetadataKey, "jkt-1"))))
	out, _ = metadata.FromOutgoingContext(twice)
	if got := out.Get(principal.DeviceMetadataKey); len(got) != 1 {
		t.Errorf("device duplicated by a second forward: %v", got)
	}
}
