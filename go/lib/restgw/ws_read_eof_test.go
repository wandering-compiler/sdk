package restgw_test

import (
	"context"
	"testing"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestWSReadProtoOrEOF_Sentinel — a TEXT frame equal to the half-close marker
// is reported as eof (the client-stream "done sending" signal), regardless of
// the negotiated wire format.
func TestWSReadProtoOrEOF_Sentinel(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.pb"}},
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = conn.Write(ctx, websocket.MessageText, []byte(restgw.WSClientStreamEOF))
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			eof, err := restgw.WSReadProtoOrEOF(ctx, conn, &got, restgw.WireProto)
			if err != nil {
				t.Fatalf("WSReadProtoOrEOF: %v", err)
			}
			if !eof {
				t.Error("half-close sentinel must report eof=true")
			}
		})
}

// TestWSReadProtoOrEOF_DecodesFrame — a non-sentinel frame decodes into msg
// (eof=false), exactly like WSReadProto.
func TestWSReadProtoOrEOF_DecodesFrame(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.pb"}},
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = restgw.WSWriteProto(ctx, conn, wrapperspb.String("payload"), restgw.WireProto)
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			eof, err := restgw.WSReadProtoOrEOF(ctx, conn, &got, restgw.WireProto)
			if err != nil || eof {
				t.Fatalf("WSReadProtoOrEOF: eof=%v err=%v", eof, err)
			}
			if got.Value != "payload" {
				t.Errorf("decoded %q, want payload", got.Value)
			}
		})
}

// TestWSReadProto_ReadError — a transport-level close surfaces as the read
// error (the generated handler translates it to gRPC EOF).
func TestWSReadProto_ReadError(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.pb"}},
		func(_ context.Context, conn *websocket.Conn) {
			_ = conn.Close(websocket.StatusInternalError, "server gone")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			if err := restgw.WSReadProto(ctx, conn, &got, restgw.WireProto); err == nil {
				t.Error("read on a server-closed conn should error")
			}
		})
}

// TestWSReadProtoOrEOF_ReadError — a transport close surfaces as err (not eof)
// so the receive loop distinguishes a real drop from the half-close sentinel.
func TestWSReadProtoOrEOF_ReadError(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.pb"}},
		func(_ context.Context, conn *websocket.Conn) {
			_ = conn.Close(websocket.StatusInternalError, "server gone")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			eof, err := restgw.WSReadProtoOrEOF(ctx, conn, &got, restgw.WireProto)
			if err == nil {
				t.Error("read on a server-closed conn should error")
			}
			if eof {
				t.Error("a transport error must not be reported as eof")
			}
		})
}
