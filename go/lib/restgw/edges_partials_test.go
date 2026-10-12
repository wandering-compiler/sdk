package restgw_test

import (
	"context"
	"encoding/base64"
	"io"
	"log"
	"testing"

	"github.com/coder/websocket"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The principal stamp is replaced, never appended: a caller-supplied
// x-w17-user already on the outgoing context cannot ride along next to the
// verified one.
func TestSetUserMetadata_ReplacesAPreexistingValue(t *testing.T) {
	seed := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-w17-user", "forged", "x-trace", "abc"))
	ctx := restgw.SetUserMetadata(seed, []byte("verified"))
	md, _ := metadata.FromOutgoingContext(ctx)
	got := md.Get("x-w17-user")
	if len(got) != 1 || got[0] != base64.StdEncoding.EncodeToString([]byte("verified")) {
		t.Fatalf("x-w17-user = %v, want exactly the verified value", got)
	}
	if v := md.Get("x-trace"); len(v) != 1 || v[0] != "abc" {
		t.Errorf("other outgoing metadata lost: %v", v)
	}
	if orig, _ := metadata.FromOutgoingContext(seed); orig.Get("x-w17-user")[0] != "forged" {
		t.Error("the caller's metadata was mutated in place")
	}
}

// A pump that panics closes its socket with StatusInternalError, and the
// client sees that status, instead of the process crashing.
func TestRecoverWSPump_ClosesWithInternalError(t *testing.T) {
	orig := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(orig)

	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			defer restgw.RecoverWSPump(ctx, conn, "Svc.Pump")
			panic("pump exploded")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			_, _, err := conn.Read(ctx)
			if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
				t.Errorf("close status = %v (%v), want StatusInternalError", got, err)
			}
		})
}
