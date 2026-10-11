package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// wsReadOnce serves one WebSocket that reads a single message and reports
// how it went, and returns a client connected to it.
func wsReadOnce(t *testing.T) (*websocket.Conn, <-chan error) {
	t.Helper()
	got := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			got <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, err = conn.Read(r.Context())
		got <- err
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetReadLimit(-1)
	t.Cleanup(func() { _ = client.CloseNow() })
	return client, got
}

func readResult(t *testing.T, got <-chan error) error {
	t.Helper()
	select {
	case err := <-got:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("server never finished reading")
		return nil
	}
}

// A client-stream or bidi frame that is a valid request body over REST must
// be a valid message over a WebSocket too. The WebSocket library's default
// read limit is 32 KiB; the gateway's request body cap is 10 MiB.
func TestAcceptWebSocket_ReadLimitIsTheRequestBodyCap(t *testing.T) {
	client, got := wsReadOnce(t)
	msg := `{"w17":"d","data":{"blob":"` + strings.Repeat("x", 512<<10) + `"}}`
	if err := client.Write(context.Background(), websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := readResult(t, got); err != nil {
		t.Fatalf("a %d-byte message (under the %d-byte body cap) was refused: %v", len(msg), restgw.MaxRequestBodyBytes, err)
	}
}

// The limit is configurable, and it IS a limit: a message over it is
// refused with "message too big".
func TestAcceptWebSocket_ReadLimitIsConfigurable(t *testing.T) {
	orig := restgw.MaxWebSocketMessageBytes
	restgw.MaxWebSocketMessageBytes = 1 << 10
	defer func() { restgw.MaxWebSocketMessageBytes = orig }()

	client, got := wsReadOnce(t)
	_ = client.Write(context.Background(), websocket.MessageText, []byte(strings.Repeat("x", 2<<10)))
	err := readResult(t, got)
	if err == nil {
		t.Fatal("a message over MaxWebSocketMessageBytes was accepted")
	}
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig && !strings.Contains(err.Error(), "read limited") {
		t.Fatalf("over-limit message failed for another reason: %v", err)
	}
}

// A translated error sentence is easily longer than the 123 bytes a close
// frame can carry. The socket must still close with a policy violation —
// not drop without a close frame, which the client sees as 1006.
func TestWSWriteErrorWithDetails_LongSentenceStillClosesCleanly(t *testing.T) {
	// 60 two-byte runes: 120 bytes of letters plus the rest — over 123
	// bytes, with a multi-byte rune straddling the cut.
	sentence := strings.Repeat("ž", 60) + " Nemáte oprávnění k tomuto záznamu."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		restgw.WSWriteErrorWithDetails(r.Context(), conn, "PERMISSION_DENIED", sentence, nil)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.CloseNow() }()

	_, frame, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("error frame never arrived: %v", err)
	}
	if !strings.Contains(string(frame), sentence) {
		t.Errorf("the error frame must carry the whole sentence; got %s", frame)
	}
	_, _, err = client.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("socket ended without a close frame (%v); the client sees 1006", err)
	}
	if ce.Code != websocket.StatusPolicyViolation {
		t.Errorf("close code = %d, want %d", ce.Code, websocket.StatusPolicyViolation)
	}
	if len(ce.Reason) > 123 {
		t.Errorf("close reason is %d bytes, over the protocol's 123", len(ce.Reason))
	}
}
