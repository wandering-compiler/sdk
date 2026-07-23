package restgw_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// T2-6 pass #5 D2: on the JSON leg a data frame and an error envelope were
// both bare TEXT JSON objects with nothing to tell them apart, so the
// generated client decoded a rejection as the response — a refused
// client-stream upload resolved as success. Frames now carry a
// discriminator; nothing may be inferred from an object's shape, since two
// unrelated types can have identical shapes.

// wsFrameKind pulls the discriminator off a raw JSON frame.
func wsFrameKind(t *testing.T, raw []byte) string {
	t.Helper()
	var env struct {
		W17 string `json:"w17"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("frame is not JSON: %v (%s)", err, raw)
	}
	return env.W17
}

func TestWSJSONDataFrameCarriesDiscriminator(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.json"}},
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = restgw.WSWriteProto(ctx, conn, wrapperspb.String("payload"), restgw.WireJSON)
		},
		func(ctx context.Context, conn *websocket.Conn) {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if kind := wsFrameKind(t, raw); kind != "d" {
				t.Errorf("data frame discriminator = %q, want \"d\" (raw: %s)", kind, raw)
			}
		})
}

func TestWSJSONErrorFrameCarriesDiscriminator(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.json"}},
		func(ctx context.Context, conn *websocket.Conn) {
			restgw.WSWriteError(ctx, conn, "INVALID_ARGUMENT", "bad input")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if kind := wsFrameKind(t, raw); kind != "e" {
				t.Errorf("error frame discriminator = %q, want \"e\" (raw: %s)", kind, raw)
			}
			// The payload stays readable — only the discriminator is terse.
			if !strings.Contains(string(raw), `"INVALID_ARGUMENT"`) {
				t.Errorf("error frame lost its code: %s", raw)
			}
		})
}

// A data frame and an error frame must never be confusable: same socket,
// same frame type, distinguishable only by the discriminator.
func TestWSJSONDataAndErrorFramesAreDistinguishable(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.json"}},
		func(ctx context.Context, conn *websocket.Conn) {
			_ = restgw.WSWriteProto(ctx, conn, wrapperspb.String("payload"), restgw.WireJSON)
			restgw.WSWriteError(ctx, conn, "INTERNAL", "boom")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			_, first, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read first: %v", err)
			}
			_, second, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read second: %v", err)
			}
			a, b := wsFrameKind(t, first), wsFrameKind(t, second)
			if a == b {
				t.Fatalf("data and error frames share discriminator %q — indistinguishable", a)
			}
			if a != "d" || b != "e" {
				t.Errorf("kinds = %q,%q; want d,e", a, b)
			}
		})
}

// Round-trip: an enveloped data frame still decodes into the message.
func TestWSJSONEnvelopedDataFrameRoundTrips(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.json"}},
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = restgw.WSWriteProto(ctx, conn, wrapperspb.String("payload"), restgw.WireJSON)
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			if err := restgw.WSReadProto(ctx, conn, &got, restgw.WireJSON); err != nil {
				t.Fatalf("WSReadProto: %v", err)
			}
			if got.Value != "payload" {
				t.Errorf("decoded %q, want payload", got.Value)
			}
		})
}

// The half-close sentinel is a discriminated frame too — its old form was a
// bare string defended as "not a JSON object, so it can't collide", which is
// the same shape-based reasoning this change removes.
func TestWSClientStreamEOFIsDiscriminated(t *testing.T) {
	if kind := wsFrameKind(t, []byte(restgw.WSClientStreamEOF)); kind != "f" {
		t.Errorf("EOF sentinel discriminator = %q, want \"f\" (raw: %s)", kind, restgw.WSClientStreamEOF)
	}
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.json"}},
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = conn.Write(ctx, websocket.MessageText, []byte(restgw.WSClientStreamEOF))
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			eof, err := restgw.WSReadProtoOrEOF(ctx, conn, &got, restgw.WireJSON)
			if err != nil {
				t.Fatalf("WSReadProtoOrEOF: %v", err)
			}
			if !eof {
				t.Error("half-close frame must report eof=true")
			}
		})
}
