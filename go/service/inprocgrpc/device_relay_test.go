package inprocgrpc_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// The verified device id reaches an in-process tier and a nested call it
// makes, as across a wire hop: the composed binary and the split deployment
// agree on principal.DeviceID at every depth.
func TestConn_NestedCall_RelaysTheDeviceID(t *testing.T) {
	conn, outer, inner := nestedCallFixture(t)

	md := gatewayMD()
	md.Set(principal.DeviceMetadataKey, "jkt-1")
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(metadata.NewOutgoingContext(context.Background(), md), relayMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for name, rec := range map[string]*mdRecorder{"first hop": outer, "nested hop": inner} {
		dev, ok := principal.DeviceID(metadata.NewIncomingContext(context.Background(), rec.incoming))
		if !ok || dev != "jkt-1" {
			t.Errorf("%s: device = (%q, %v), want jkt-1", name, dev, ok)
		}
	}
}

// A caller with the device only on its INCOMING side (a handler serving a
// gateway request, calling another tier in-process) relays it, as the
// dialled-conn interceptor would.
func TestConn_IncomingDeviceIsRelayed(t *testing.T) {
	conn := inprocgrpc.New()
	rec := &mdRecorder{}
	conn.RegisterService(unaryDesc("test.Record", "Record", rec.serve), nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(mdUser, "env", principal.DeviceMetadataKey, "jkt-2"))
	if err := conn.Invoke(ctx, recordMethod, wrapperspb.String("x"), new(wrapperspb.StringValue)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := rec.incoming.Get(principal.DeviceMetadataKey); len(got) != 1 || got[0] != "jkt-2" {
		t.Errorf("device = %v, want [jkt-2]", got)
	}
}
