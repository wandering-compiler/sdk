package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestWS_ProtobufRoundTrip drives the server WS helpers exactly the
// way a generated server-stream handler does, over a real WebSocket:
// AcceptWebSocket advertises the subprotocols, WSFormat reads back
// the negotiated one, WSReadProto decodes the binary request, and
// WSWriteProto streams two binary responses. The client connects
// with the w17.pb subprotocol and exchanges MessageBinary frames —
// proving the server's WS binary path end-to-end.
func TestWS_ProtobufRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		format := restgw.WSFormat(conn)
		if format != restgw.WireProto {
			t.Errorf("server WSFormat = %v, want WireProto", format)
		}
		req := &wrapperspb.StringValue{}
		if err := restgw.WSReadProto(r.Context(), conn, req, format); err != nil {
			t.Errorf("server WSReadProto: %v", err)
			return
		}
		// Echo the decoded value back twice as a 2-message stream.
		for i := 0; i < 2; i++ {
			if err := restgw.WSWriteProto(r.Context(), conn, wrapperspb.String(req.GetValue()+"-echo"), format); err != nil {
				t.Errorf("server WSWriteProto: %v", err)
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + srv.URL[len("http"):]
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"w17.pb"},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if conn.Subprotocol() != "w17.pb" {
		t.Fatalf("negotiated subprotocol = %q, want w17.pb", conn.Subprotocol())
	}

	// Send the request as one binary protobuf frame.
	reqBytes, _ := proto.Marshal(wrapperspb.String("hello"))
	if err := conn.Write(ctx, websocket.MessageBinary, reqBytes); err != nil {
		t.Fatalf("client write: %v", err)
	}

	// Read the two binary response frames back.
	for i := 0; i < 2; i++ {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("client read %d: %v", i, err)
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("frame %d type = %s, want binary", i, typ)
		}
		got := &wrapperspb.StringValue{}
		if err := proto.Unmarshal(data, got); err != nil {
			t.Fatalf("frame %d not valid protobuf: %v", i, err)
		}
		if got.GetValue() != "hello-echo" {
			t.Errorf("frame %d value = %q, want hello-echo", i, got.GetValue())
		}
	}
}
