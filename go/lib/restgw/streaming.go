package restgw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/wandering-compiler/sdk/go/core/observx"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrStreamTerminalErrorSent is the cancellation cause a bidi WS handler's
// inbound pump sets (via context.WithCancelCause) when it has ALREADY
// delivered a terminal error frame to the client (e.g. a per-frame
// validation failure) before tearing down the stream context. The
// outbound recv loop checks context.Cause(ctx) against it (Q66-stream-1):
// the loop's own stream.Recv() then returns context.Canceled, and without
// this signal it would write a SECOND, misleading "canceled" error frame
// on top of the terminal one the pump already sent. A real backend error
// leaves the cause unset, so it still surfaces normally.
var ErrStreamTerminalErrorSent = errors.New("restgw: terminal stream error already delivered to client")

// SSE (Server-Sent Events) helpers — server → client only.
// Generated SSE handlers call WriteSSEHeaders once at the
// start, then WriteSSEEvent per gRPC server-stream message
// until the stream EOFs (or the client disconnects).
//
// WS (WebSocket) helpers — bidirectional. Used by:
//   - server-stream RPCs whose author flipped to WS via
//     `(w17.http).stream_mode = WS` (browser-friendly).
//   - client-stream RPCs (HTTP→gRPC client side runs Recv in
//     a loop on the WS, gRPC sends via stream.Send).
//   - bidi-stream RPCs (both directions pumped concurrently).
//
// The gateway uses `github.com/coder/websocket` (the modern
// fork of nhooyr/websocket; pinned in go.mod). Generated
// code calls Accept + ReadProto / WriteProto helpers below;
// no per-handler boilerplate.

// WriteSSEHeaders writes the SSE response headers + flushes
// them so the client sees the connection is open before the
// first event arrives. Returns (nil, true) on success.
// Returns (nil, false) when the response writer doesn't
// implement http.Flusher — generated handlers WriteError
// 500 in that case (rare; net/http's default writers all
// flush, but reverse proxies can wrap the writer).
//
// The "no-cache" header pair stops intermediaries from
// buffering events; "Connection: keep-alive" is informational
// (HTTP/1.1 keeps connections alive by default) but kept for
// proxies that pre-date that default.
func WriteSSEHeaders(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

// WriteSSEEvent marshals msg to JSON and writes one SSE
// event in the standard `data: <json>\n\n` shape, then
// flushes. Returns the underlying io error on partial
// writes (client disconnect mid-event); generated handlers
// abort the stream loop on the first error.
//
// Error events ride the same channel — see [WriteSSEError]
// for the canonical envelope.
//
// Back-pressure: an HTTP write to a slow client blocks
// until the kernel accepts the bytes, which propagates
// pressure back to the upstream gRPC stream's send buffer.
// The chain is bounded: TCP send buffer (kernel) →
// http.ResponseWriter (no internal queue) → SSE handler
// goroutine. If the client stalls past a threshold the
// goroutine pins indefinitely. Use [WriteSSEEventWithTimeout]
// to bound write latency (G3-GW-09).
func WriteSSEEvent(w http.ResponseWriter, flusher http.Flusher, msg proto.Message) error {
	// Build the whole `data: <json>\n\n` frame in ONE pooled buffer so a
	// registered generated marshaler makes the frame zero-allocation (the
	// marshaler appends the JSON straight after the `data: ` prefix). Our
	// JSON is always single-line, so one data field per event.
	bp := jsonBufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	buf = append(buf, "data: "...)
	buf, err := MarshalProtoAppend(buf, msg)
	if err != nil {
		*bp = buf
		jsonBufPool.Put(bp)
		return fmt.Errorf("marshal SSE event: %w", err)
	}
	buf = append(buf, '\n', '\n')
	_, werr := w.Write(buf)
	*bp = buf
	jsonBufPool.Put(bp)
	if werr != nil {
		return werr
	}
	flusher.Flush()
	return nil
}

// WriteSSEEventWithTimeout (G3-GW-09) wraps WriteSSEEvent
// with a per-write deadline via [http.NewResponseController].
// When the underlying TCP send buffer can't drain within
// `timeout`, the write returns a deadline error; the caller
// treats it like any other client-disconnect — abort the
// stream loop, free the goroutine.
//
// `timeout` of 0 disables the deadline (same as WriteSSEEvent).
// Operators tune via env var on the gateway main.go (typical
// 5-30s; longer = more memory tolerance for clients on slow
// links, shorter = tighter back-pressure).
//
// Note: ResponseController is Go 1.20+. The deadline is
// installed before the write and cleared after — back-to-
// back calls each get a fresh budget.
func WriteSSEEventWithTimeout(w http.ResponseWriter, flusher http.Flusher, msg proto.Message, timeout time.Duration) error {
	if timeout > 0 {
		ctrl := http.NewResponseController(w)
		if err := ctrl.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			// Some ResponseWriter wrappers don't support
			// SetWriteDeadline (e.g. test recorders, certain
			// reverse-proxy wrappers). Fall back to the
			// no-deadline path so the write still works,
			// just without back-pressure bound.
			_ = err
		} else {
			defer func() {
				_ = ctrl.SetWriteDeadline(time.Time{})
			}()
		}
	}
	return WriteSSEEvent(w, flusher, msg)
}

// WriteSSEError emits one SSE event whose payload is the
// canonical error envelope (`{"error": {"code": ...,
// "message": ...}}`). Used when the gRPC backend returns a
// status mid-stream; the server-stream gRPC contract surfaces
// errors only on stream.Recv, not as a side channel, so the
// gateway has to translate them onto the SSE wire as a
// regular event.
func WriteSSEError(w http.ResponseWriter, flusher http.Flusher, code, message string) {
	body := errorEnvelopeJSON(code, message, nil)
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", body)
	flusher.Flush()
}

// WriteSSEGRPCError unpacks a gRPC error to (code, message)
// and emits it as an SSE error event. Generated handlers
// call this when stream.Recv returns a non-EOF error;
// non-status errors (network blip) fall through to
// "INTERNAL".
func WriteSSEGRPCError(w http.ResponseWriter, flusher http.Flusher, err error) {
	if err == nil {
		return
	}
	if st, ok := status.FromError(err); ok {
		WriteSSEError(w, flusher, GRPCCodeName(st.Code()), scrubTransportMessage(st, err))
		return
	}
	// Non-status (transport / unexpected): its text carries internal
	// topology, so it is logged and genericised — the unary path's
	// restgw-sec-3 posture, which this leg was emitting raw.
	observx.ReportError(context.Background(), err)
	WriteSSEError(w, flusher, "INTERNAL", "internal error")
}

// WriteSSEGRPCErrorWithTimeout wraps WriteSSEGRPCError with a per-write
// deadline (G3-GW-09 error-path sibling, Q5-gw-1) so the terminal error
// frame can't pin the handler goroutine on a half-open client. timeout
// of 0 disables the bound (same as WriteSSEGRPCError).
func WriteSSEGRPCErrorWithTimeout(w http.ResponseWriter, flusher http.Flusher, err error, timeout time.Duration) {
	if timeout > 0 {
		ctrl := http.NewResponseController(w)
		if derr := ctrl.SetWriteDeadline(time.Now().Add(timeout)); derr == nil {
			defer func() { _ = ctrl.SetWriteDeadline(time.Time{}) }()
		}
	}
	WriteSSEGRPCError(w, flusher, err)
}

// AcceptWebSocket upgrades the HTTP request to a WebSocket
// connection with the project's standard options:
//   - subprotocols: advertises `w17.pb` + `w17.json` (C9) so a
//     browser client — which can't set request headers on a WS
//     upgrade — negotiates the wire format via the subprotocol.
//     coder/websocket echoes the first advertised protocol the
//     client also offered; read it back via [WSFormat]. A client
//     that offers neither gets the empty subprotocol → JSON.
//   - InsecureSkipVerify: false (default) — same-origin
//     enforced; consumers behind a reverse proxy set
//     `OriginPatterns` via the option helper if needed.
//   - CompressionMode: disabled — tiny per-message payloads
//     don't benefit, deflate adds CPU.
//
// On accept failure (handshake rejection, origin mismatch)
// the helper has already responded; caller bails without
// further writes.
func AcceptWebSocket(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		Subprotocols:    []string{wsSubprotocolPB, wsSubprotocolJSON},
	})
}

// WSFormat reports the wire format negotiated for a WS conn from
// the accepted subprotocol (C9). `w17.pb` → WireProto; anything
// else (including the empty subprotocol a JSON / legacy client
// leaves) → WireJSON. Generated WS handlers compute this once
// after AcceptWebSocket and thread it into the read/write pumps.
func WSFormat(conn *websocket.Conn) WireFormat {
	if conn != nil && conn.Subprotocol() == wsSubprotocolPB {
		return WireProto
	}
	return WireJSON
}

// wsFormatOpt resolves a WS frame format from the optional trailing
// arg (JSON when absent) — keeps WSReadProto / WSWriteProto
// backward compatible with pre-C9 generated callers.
func wsFormatOpt(fmtOpt []WireFormat) WireFormat {
	if len(fmtOpt) > 0 {
		return fmtOpt[0]
	}
	return WireJSON
}

// WSReadProto reads one WS message and unmarshals it into msg in
// the negotiated format (C9): a binary frame via proto.Unmarshal
// when the format is WireProto, otherwise a text frame via the JSON
// path. The format arg is variadic (pass at most one) for backward
// compatibility — pre-C9 callers (`WSReadProto(ctx, conn, msg)`)
// keep the text/JSON behaviour. Returns the underlying error on
// connection close / decode failure; generated handlers translate
// close errors to gRPC EOF and decode errors to a status
// `INVALID_ARGUMENT` close code.
func WSReadProto(ctx context.Context, conn *websocket.Conn, msg proto.Message, fmtOpt ...WireFormat) error {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return wsDecodeFrame(typ, data, msg, wsFormatOpt(fmtOpt))
}

// Every JSON frame we put on a WS socket carries the reserved "w17"
// discriminator naming its kind. Nothing about a frame may be inferred
// from its shape: a data frame and an error envelope are both JSON
// objects on the same socket, and two unrelated messages can serialise
// to the same shape, so shape-sniffing is wrong in principle — it was
// how a refused client-stream upload got decoded as its own success
// response. Values are single letters because the JSON leg is the
// secondary transport (binary is primary) and no human reads the frames;
// the payload keys stay spelled out so a devtools inspection still shows
// {code, message, details}.
//
// The binary leg is unaffected: its data frames are protobuf, not JSON,
// and the frame TYPE (binary vs text) is a real out-of-band
// discriminator rather than a guess. Its error envelope is JSON, so it
// gets the marker too.
const (
	wsKindData  = `{"w17":"d","data":`
	wsKindError = `{"w17":"e","error":`
)

// WSClientStreamEOF is the half-close sentinel a client-stream client
// sends to mark "done sending" WITHOUT closing the socket — WebSocket has
// no native half-close, and a client that closes the socket would never
// receive the single response the server replies with afterward (once
// close() runs the socket is CLOSING and inbound message frames are
// dropped, in browsers and Node alike). So the generated client sends this
// reserved TEXT frame instead; the server recognises it, drains the gRPC
// stream, writes the response on the still-open socket, then closes.
// The generated TS/JS client emits the identical byte sequence.
const WSClientStreamEOF = `{"w17":"f"}`

// WSReadProtoOrEOF is WSReadProto for the client-stream receive loop: it
// reports eof=true when the inbound frame is the WSClientStreamEOF
// half-close sentinel (a TEXT frame equal to the marker, regardless of the
// negotiated wire format), otherwise it decodes the frame into msg exactly
// like WSReadProto. A transport error (real close / drop) surfaces as err.
func WSReadProtoOrEOF(ctx context.Context, conn *websocket.Conn, msg proto.Message, fmtOpt ...WireFormat) (eof bool, err error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return false, err
	}
	if typ == websocket.MessageText && string(data) == WSClientStreamEOF {
		return true, nil
	}
	return false, wsDecodeFrame(typ, data, msg, wsFormatOpt(fmtOpt))
}

// wsDecodeFrame unmarshals one already-read WS frame into msg per the
// negotiated format: a binary frame via proto.Unmarshal for WireProto,
// otherwise a text frame via the JSON path. Shared by WSReadProto and
// WSReadProtoOrEOF.
func wsDecodeFrame(typ websocket.MessageType, data []byte, msg proto.Message, format WireFormat) error {
	if format == WireProto {
		if typ != websocket.MessageBinary {
			return fmt.Errorf("ws: expected binary frame, got %s", typ)
		}
		if err := proto.Unmarshal(data, msg); err != nil {
			return fmt.Errorf("ws: invalid protobuf: %w", err)
		}
		return nil
	}
	if typ != websocket.MessageText {
		return fmt.Errorf("ws: expected text frame, got %s", typ)
	}
	payload, err := wsUnwrapJSONFrame(data)
	if err != nil {
		return err
	}
	return UnmarshalProto(payload, msg)
}

// wsUnwrapJSONFrame strips the discriminating envelope off an inbound JSON
// frame and returns the raw payload. A frame without the marker is
// REJECTED rather than decoded as data: accepting it would restore the
// ambiguity the envelope exists to remove.
//
// The prefix compare is the fast path for our own emitters (key order is
// fixed); anything else falls back to a real parse so a hand-written or
// spec-generated client that orders keys differently still works.
func wsUnwrapJSONFrame(data []byte) ([]byte, error) {
	if bytes.HasPrefix(data, []byte(wsKindData)) && bytes.HasSuffix(data, []byte("}")) {
		return data[len(wsKindData) : len(data)-1], nil
	}
	var env struct {
		W17   string          `json:"w17"`
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("ws: frame is not a JSON envelope: %w", err)
	}
	switch env.W17 {
	case "d":
		return env.Data, nil
	case "e":
		return nil, fmt.Errorf("ws: peer sent an error frame: %s", env.Error)
	case "":
		return nil, fmt.Errorf("ws: frame is missing the w17 discriminator")
	default:
		return nil, fmt.Errorf("ws: unknown frame kind %q", env.W17)
	}
}

// WSWriteProto marshals msg in the negotiated format (C9) and
// writes it as one WS message: a binary frame (proto.Marshal)
// when format is WireProto, otherwise a text JSON frame. Errors
// are propagated to the generated handler which closes the conn.
//
// Back-pressure: coder/websocket's Write respects ctx
// cancellation, so passing a derived ctx with a timeout
// bounds the write latency. See [WSWriteProtoWithTimeout]
// for the bounded variant (G3-GW-09).
func WSWriteProto(ctx context.Context, conn *websocket.Conn, msg proto.Message, fmtOpt ...WireFormat) error {
	if wsFormatOpt(fmtOpt) == WireProto {
		// Pooled marshal → write → release, mirroring writeNegotiated's PB
		// branch + the JSON path below: conn.Write is synchronous, so the
		// buffer carries no live reference once it returns and can be recycled
		// (a binary server-stream would otherwise allocate a fresh marshal
		// buffer per message — exactly the garbage the pool exists to avoid).
		bp := pbMarshalBufPool.Get().(*[]byte)
		body, err := proto.MarshalOptions{}.MarshalAppend((*bp)[:0], msg)
		if err != nil {
			*bp = body
			pbMarshalBufPool.Put(bp)
			return fmt.Errorf("ws: marshal: %w", err)
		}
		werr := conn.Write(ctx, websocket.MessageBinary, body)
		*bp = body
		pbMarshalBufPool.Put(bp)
		return werr
	}
	// Pooled marshal → write → release (conn.Write writes the frame
	// synchronously, so the buffer is free once it returns). Zero-alloc
	// for a registered generated marshaler.
	bp := jsonBufPool.Get().(*[]byte)
	// Envelope is built into the SAME pooled buffer as the payload:
	// prefix, marshal in place, close the brace. No extra allocation.
	body := append((*bp)[:0], wsKindData...)
	body, err := MarshalProtoAppend(body, msg)
	if err != nil {
		*bp = body
		jsonBufPool.Put(bp)
		return fmt.Errorf("ws: marshal: %w", err)
	}
	body = append(body, '}')
	werr := conn.Write(ctx, websocket.MessageText, body)
	*bp = body
	jsonBufPool.Put(bp)
	return werr
}

// WSWriteProtoWithTimeout (G3-GW-09) wraps WSWriteProto
// with a derived ctx that has a per-write deadline. A slow
// client whose receive window stalls past `timeout` gets
// the write aborted with `context.DeadlineExceeded`; the
// generated handler closes the conn and frees the
// pumping goroutine.
//
// `timeout` of 0 disables the bound (same as WSWriteProto).
// Operators tune via env var on the gateway main.go.
//
// Distinct from the request ctx: a long-lived bidi WS
// session might run for minutes; the request ctx applies
// to the entire connection while this timeout bounds each
// individual write.
func WSWriteProtoWithTimeout(ctx context.Context, conn *websocket.Conn, msg proto.Message, timeout time.Duration, fmtOpt ...WireFormat) error {
	if timeout <= 0 {
		return WSWriteProto(ctx, conn, msg, fmtOpt...)
	}
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return WSWriteProto(writeCtx, conn, msg, fmtOpt...)
}

// defaultStreamWriteTimeout bounds each SSE/WS frame write on a
// generated per-RPC streaming handler — the same goroutine-leak
// guard the reserved w17-events channel applies (see
// defaultW17EventsWriteTimeout). Generous enough for slow links,
// short enough to reclaim a goroutine stuck on a half-open client.
const defaultStreamWriteTimeout = 30 * time.Second

// StreamWriteTimeoutFromEnv reads `<PREFIX>_STREAM_WRITE_TIMEOUT_SECONDS`
// off `lookup` and returns the per-frame write deadline the generated
// SSE/WS handlers pass to [WriteSSEEventWithTimeout] /
// [WSWriteProtoWithTimeout] (G3-GW-09). Unset / unparseable falls back
// to defaultStreamWriteTimeout (30s) so out-of-the-box bundles are
// bounded; an explicit value <= 0 disables the deadline (operator
// opt-out, matching the WithTimeout `timeout == 0` passthrough).
func StreamWriteTimeoutFromEnv(prefix string, lookup func(string) string) time.Duration {
	raw := lookup(prefix + "_STREAM_WRITE_TIMEOUT_SECONDS")
	if raw == "" {
		return defaultStreamWriteTimeout
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultStreamWriteTimeout
	}
	if n <= 0 {
		return 0 // explicit opt-out — no deadline
	}
	return time.Duration(n) * time.Second
}

// WSWriteError sends the canonical error envelope as a text
// frame and then closes the conn with StatusPolicyViolation
// (or StatusInternalError for non-status backend failures).
// Mirrors WriteError on the unary HTTP side.
func WSWriteError(ctx context.Context, conn *websocket.Conn, code, message string) {
	WSWriteErrorWithDetails(ctx, conn, code, message, nil)
}

// WSWriteErrorWithDetails sends the structured error envelope
// (`{error: {code, message, details: [...]}}`) as a text
// frame and then closes the conn. Used by generated WS
// handlers for per-frame validation rejection (G3-V-02) so the
// caller learns which fields were invalid before the WS
// terminates. Mirrors [WriteErrorWithDetails] on the unary
// HTTP side.
func WSWriteErrorWithDetails(ctx context.Context, conn *websocket.Conn, code, message string, details []FieldError) {
	body := wsErrorFrameJSON(code, message, details)
	_ = conn.Write(ctx, websocket.MessageText, body)
	_ = conn.Close(websocket.StatusPolicyViolation, message)
}

// WSWriteErrorWithDetailsWithTimeout wraps WSWriteErrorWithDetails with a
// derived ctx deadline (Q5-gw-1) so the validation-rejection frame + close
// can't pin the goroutine. timeout of 0 disables the bound.
func WSWriteErrorWithDetailsWithTimeout(ctx context.Context, conn *websocket.Conn, code, message string, details []FieldError, timeout time.Duration) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	WSWriteErrorWithDetails(ctx, conn, code, message, details)
}

// WSWriteGRPCError unpacks a gRPC error and sends it as a
// WS error envelope before closing the conn. Same dispatch
// as WriteSSEGRPCError on the SSE side.
func WSWriteGRPCError(ctx context.Context, conn *websocket.Conn, err error) {
	if err == nil {
		return
	}
	if st, ok := status.FromError(err); ok {
		// B25-restgw-1: forward the backend's field-violation details (parity
		// with the unary WriteGRPCError path).
		if details := fieldErrorsFromStatus(st); len(details) > 0 {
			WSWriteErrorWithDetails(ctx, conn, GRPCCodeName(st.Code()), st.Message(), details)
			return
		}
		WSWriteError(ctx, conn, GRPCCodeName(st.Code()), scrubTransportMessage(st, err))
		return
	}
	// Non-status (transport / unexpected): logged + genericised, matching
	// the unary path (restgw-sec-3). This leg was emitting it raw.
	observx.ReportError(ctx, err)
	WSWriteError(ctx, conn, "INTERNAL", "internal error")
}

// WSWriteGRPCErrorWithTimeout wraps WSWriteGRPCError with a derived ctx
// deadline (Q5-gw-1) so the terminal error frame + close can't pin the
// goroutine. timeout of 0 disables the bound.
func WSWriteGRPCErrorWithTimeout(ctx context.Context, conn *websocket.Conn, err error, timeout time.Duration) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	WSWriteGRPCError(ctx, conn, err)
}
