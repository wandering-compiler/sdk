package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

// ErrStreamClosed reports that a stream ended — the server finished sending
// and closed the connection. It is a NORMAL outcome, and telling it apart
// from a timeout is the point: "the stream ended after three frames" and
// "the stream went quiet after three frames" are different servers, and a
// reader that reported both the same way would pass the second one.
var ErrStreamClosed = errors.New("stream closed")

// ExpectStream is a step's `expect_stream:` block — the assertion that an
// endpoint's own stream (a server-streaming RPC routed as SSE) sends a
// particular SEQUENCE of frames.
//
// It exists because a declared streaming endpoint had no caller anywhere in
// the corpus. It was generated, it compiled, and nothing had ever driven
// one — the shape that had already cost this repo twice, where a surface no
// example exercises is a surface whose defects wait for a consumer.
//
// `await_events` is the nearest neighbour and a different thing: it
// subscribes to the project's public event bus around a call. This opens the
// ENDPOINT and reads what the endpoint itself sends.
type ExpectStream struct {
	// Frames is the ordered, EXHAUSTIVE contract: frame i must match entry
	// i, using the same matcher vocabulary as Step.Expect.
	//
	// Exhaustive on purpose. A stream assertion with a lower bound ("at
	// least one frame arrived") asserts nothing: it passes for a server
	// that sends one frame and hangs, which is the most likely way a
	// streaming endpoint actually breaks. The count and the order ARE the
	// contract.
	Frames []map[string]any

	// AllowMore relaxes the end of the stream, and only the end. With it
	// false — the default — the stream must CLOSE after the last listed
	// frame, and a server that keeps sending fails.
	//
	// An endless feed sets it true, which reinstates the lower bound above
	// deliberately and visibly, in the one place a reader will look.
	AllowMore bool

	// TimeoutMs bounds the wait for EACH frame. Zero → DefaultAwaitTimeoutMs.
	TimeoutMs int
}

// StreamConn is one open endpoint stream.
type StreamConn interface {
	// Next returns the next frame in arrival order, or ErrStreamClosed
	// when the server finished.
	Next(ctx context.Context, timeout time.Duration) (Event, error)
	Close() error
}

// StreamCaller opens an endpoint's stream. The generated runner wires an
// [SSEStreamCaller]; tests substitute a fake.
type StreamCaller interface {
	OpenStream(ctx context.Context, ep Endpoint, input map[string]any, token string, headers map[string]string) (StreamConn, error)
}

// SSEStreamCaller opens a server-streaming endpoint over SSE.
//
// Routing is [ResolveREST], the same resolver the unary caller uses — a
// stream's request message is sent once at connect, so its fields bind into
// the path and query exactly as a GET's do. Sharing the resolver is what
// keeps a path template from meaning two different things depending on
// whether the method streams.
type SSEStreamCaller struct {
	BaseURL string
	Client  *http.Client
}

// NewSSEStreamCaller builds a stream caller against baseURL. A nil client
// gets a timeout-less one: the connection is long-lived by definition, so
// the per-frame deadline bounds the wait, never the client.
func NewSSEStreamCaller(baseURL string, client *http.Client) *SSEStreamCaller {
	if client == nil {
		client = &http.Client{}
	}
	return &SSEStreamCaller{BaseURL: strings.TrimRight(baseURL, "/"), Client: client}
}

func (c *SSEStreamCaller) OpenStream(ctx context.Context, ep Endpoint, input map[string]any, token string, headers map[string]string) (StreamConn, error) {
	_, target, _, err := ResolveREST(c.BaseURL, ep, input)
	if err != nil {
		return nil, fmt.Errorf("open stream %s: %w", ep.Ref, err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open stream %s: build request: %w", ep.Ref, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Same rule as the unary caller and the event subscriber: a step's own
	// headers may not override the credential or the content negotiation.
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Accept") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open stream %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		cancel()
		// A refused connect is an ordinary REST refusal — same envelope,
		// same canonical code — so it is reported as one. That is what
		// lets a stream's `expect_error:` case assert PERMISSION_DENIED
		// with the vocabulary every other refusal case already uses.
		return nil, newCallError("open stream "+ep.Ref, resp.StatusCode, raw)
	}
	sub := &sseSub{
		cancel: cancel,
		body:   resp.Body,
		frames: make(chan Event, 16),
		done:   make(chan struct{}),
	}
	go sub.read()
	return sub, nil
}

// runStreamStep opens a streaming endpoint and asserts what it sends.
//
// A stream has two ways to refuse, and `expect_error` has to mean both:
//
//   - the CONNECT is refused — the gateway turned the request away before
//     the stream opened (no route, no credential) and answered an ordinary
//     REST error;
//   - the stream OPENED and its first frame is `event: error`. That is how
//     every refusal the BACKEND makes arrives: the gateway has already sent
//     `200 text/event-stream` by the time the handler runs, so the code and
//     details can only travel as a frame.
//
// Only the first used to count. A refusal in the handler — scope, ACL,
// overload — read as "the stream opened", so its `expect_error` case
// reported SUCCEEDED against a server that had answered correctly, and the
// whole class could not be asserted at all.
//
// The frames, when declared, come FIRST: `expect_stream` + `expect_error`
// is a stream that sends its declared frames and then fails, which is how a
// failure partway through a long job looks.
func runStreamStep(ctx context.Context, s Step, streams StreamCaller, input map[string]any, token string, headers map[string]string, scope *runtime.Scope) error {
	if s.ExpectTransportError != nil {
		return fmt.Errorf("step asserts expect_transport_error on the stream %s — the runner has no transport-level refusal to read on a stream", s.Endpoint.Ref)
	}
	if s.Credential != "" {
		// The stream caller presents the scenario's bearer or nothing, and a
		// step naming its own credential drops the bearer — so the stream
		// would open UNAUTHENTICATED, and a refusal case would pass on the
		// missing credential instead of the one under test. Codegen refuses
		// this; a hand-built step gets the same answer.
		return fmt.Errorf("step names a credential on the stream %s — the stream caller cannot present one, so the stream would open unauthenticated", s.Endpoint.Ref)
	}
	if s.ExpectStream == nil && s.ExpectError == nil {
		// Codegen refuses this shape; a hand-built step gets the same answer
		// rather than a pass for a stream nobody read.
		return fmt.Errorf("step calls the stream %s without expect_stream or expect_error — a stream case has to assert its frames or its refusal", s.Endpoint.Ref)
	}
	conn, err := streams.OpenStream(ctx, s.Endpoint, input, token, headers)
	if err != nil {
		// A refused connect satisfies a refusal case only when no frame was
		// declared before it: with frames, the case says the stream OPENS.
		if s.ExpectError != nil && s.ExpectStream == nil {
			return matchCallError(s.ExpectError, err, scope)
		}
		return err
	}
	defer func() { _ = conn.Close() }()

	timeout := DefaultAwaitTimeoutMs * time.Millisecond
	if s.ExpectStream != nil && s.ExpectStream.TimeoutMs > 0 {
		timeout = time.Duration(s.ExpectStream.TimeoutMs) * time.Millisecond
	}
	if s.ExpectStream != nil {
		if err := matchFrames(ctx, s.ExpectStream.Frames, conn, timeout, scope); err != nil {
			return err
		}
	}
	if s.ExpectError != nil {
		if s.ExpectError.TimeoutMs > 0 {
			timeout = time.Duration(s.ExpectError.TimeoutMs) * time.Millisecond
		}
		return matchStreamError(ctx, s.ExpectError, conn, timeout, scope)
	}
	if s.ExpectStream.AllowMore {
		return nil
	}
	return matchStreamClose(ctx, len(s.ExpectStream.Frames), conn, timeout)
}

// matchFrames asserts the declared frame sequence, in order.
func matchFrames(ctx context.Context, frames []map[string]any, conn StreamConn, timeout time.Duration, scope *runtime.Scope) error {
	for i, want := range frames {
		ev, err := conn.Next(ctx, timeout)
		if errors.Is(err, ErrStreamClosed) {
			return fmt.Errorf("expect_stream: the stream ended after %d frame(s), but %d were declared — frame[%d] never arrived", i, len(frames), i)
		}
		var ce *CallError
		if errors.As(err, &ce) {
			return fmt.Errorf("expect_stream: frame[%d]: the stream answered an error frame instead — %s: %s%s", i, ce.Code, ce.Message, detailsSuffix(ce))
		}
		if err != nil {
			return fmt.Errorf("expect_stream: frame[%d]: %w", i, err)
		}
		if err := runtime.MatchExpect(want, ev.Data, scope); err != nil {
			return fmt.Errorf("expect_stream: frame[%d]: %w", i, err)
		}
	}
	return nil
}

// matchStreamClose asserts the stream ENDS after its declared frames.
//
// The close is half the contract. Without it, every case would also pass
// against a server that sends the declared frames and then holds the
// connection open forever — and an endpoint that never finishes is one no
// client can use. An error frame here is a failure too: a stream whose
// frames all arrived and that then reported an error did not succeed.
func matchStreamClose(ctx context.Context, declared int, conn StreamConn, timeout time.Duration) error {
	ev, err := conn.Next(ctx, timeout)
	var ce *CallError
	switch {
	case errors.Is(err, ErrStreamClosed):
		return nil
	case errors.As(err, &ce):
		return fmt.Errorf("expect_stream: after %d declared frame(s) the stream ended with an error frame — %s: %s%s (assert it with expect_error if the failure is the case)", declared, ce.Code, ce.Message, detailsSuffix(ce))
	case err != nil:
		return fmt.Errorf("expect_stream: after %d declared frame(s) the stream neither sent another nor closed: %w (set allow_more if this feed is endless)", declared, err)
	default:
		return fmt.Errorf("expect_stream: the stream sent a frame after the %d declared ones: %v (set allow_more if this feed is endless)", declared, ev.Data)
	}
}

// matchStreamError asserts that the stream's next frame is the declared
// refusal. A data frame there and a stream that simply ends are both the
// guard letting the caller through, and each says which it was.
func matchStreamError(ctx context.Context, want *ExpectError, conn StreamConn, timeout time.Duration, scope *runtime.Scope) error {
	ev, err := conn.Next(ctx, timeout)
	var ce *CallError
	switch {
	case errors.As(err, &ce):
		return matchCallError(want, ce, scope)
	case errors.Is(err, ErrStreamClosed):
		return fmt.Errorf("expected the stream to be refused with %s, but it ENDED without an error frame — the guard under test let this caller through", want.Code)
	case err != nil:
		return fmt.Errorf("expected an error frame carrying %s, but the stream went quiet: %w", want.Code, err)
	default:
		return fmt.Errorf("expected the stream to be refused with %s, but it SENT a frame: %v — the guard under test let this caller through", want.Code, ev.Data)
	}
}

// streamErrorFrame decodes an `event: error` frame into the same CallError a
// refused unary call produces. The frame's payload IS the canonical error
// envelope, so one decoder serves both and `expect_error` reads one shape.
//
// The RAW text is decoded, not Data re-encoded: a frame that is not our
// envelope (a proxy's error, a plain-text panic) then keeps its words in the
// failure, the way newCallError keeps a non-envelope body.
func streamErrorFrame(ev Event) *CallError {
	raw := []byte(ev.Raw)
	if len(raw) == 0 {
		raw, _ = json.Marshal(ev.Data)
	}
	return newCallError("stream error frame", http.StatusOK, raw)
}

func detailsSuffix(ce *CallError) string {
	if len(ce.Details) == 0 {
		return ""
	}
	return " (details: " + describeGotDetails(ce.Details) + ")"
}
