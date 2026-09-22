package runner

import (
	"context"
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

// matchStream asserts the frame sequence an open stream delivers.
func matchStream(ctx context.Context, es *ExpectStream, conn StreamConn, scope *runtime.Scope) error {
	timeout := time.Duration(es.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = DefaultAwaitTimeoutMs * time.Millisecond
	}
	for i, want := range es.Frames {
		ev, err := conn.Next(ctx, timeout)
		if errors.Is(err, ErrStreamClosed) {
			return fmt.Errorf("expect_stream: the stream ended after %d frame(s), but %d were declared — frame[%d] never arrived", i, len(es.Frames), i)
		}
		if err != nil {
			return fmt.Errorf("expect_stream: frame[%d]: %w", i, err)
		}
		if err := runtime.MatchExpect(want, ev.Data, scope); err != nil {
			return fmt.Errorf("expect_stream: frame[%d]: %w", i, err)
		}
	}
	if es.AllowMore {
		return nil
	}
	// The close is half the contract. Without this, every case here would
	// also pass against a server that sends the declared frames and then
	// holds the connection open forever — and an endpoint that never
	// finishes is one no client can use.
	ev, err := conn.Next(ctx, timeout)
	switch {
	case errors.Is(err, ErrStreamClosed):
		return nil
	case err != nil:
		return fmt.Errorf("expect_stream: after %d declared frame(s) the stream neither sent another nor closed: %w (set allow_more if this feed is endless)", len(es.Frames), err)
	default:
		return fmt.Errorf("expect_stream: the stream sent a frame after the %d declared ones: %v (set allow_more if this feed is endless)", len(es.Frames), ev.Data)
	}
}
