// Package eventbus is the runtime-side surface for the
// compiler-generated emit interceptors (Phase 8) and
// subscriber dispatchers (Phase 9). Phase 8 introduces the
// Dispatcher contract — the small interface every transport
// adapter implements + every generated emit helper calls
// into. Phase 10 ships concrete implementations
// (in-memory bufconn, NATS, Redis Streams).
//
// Per spec docs/specs/eventbus/emit.md, emit semantics are
// fire-and-forget on RPC return: the interceptor builds the
// event payload, wraps it in the envelope, and hands it to
// the bus; failures inside the bus do NOT fail the RPC (the
// caller has already received the response). The Dispatcher
// surface reflects this — implementations return errors for
// observability (metrics, logs) but generated emit helpers
// swallow them with `_ =` so the success path stays linear.
package eventbus

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// Dispatcher routes one event envelope to the named channel +
// topic. Implementations bind to a specific transport at boot
// time (NATS JetStream, Redis Streams, in-process MEMORY).
//
// The envelope argument is the per-domain
// `<Domain>EventEnvelope` carrying the event in its oneof
// branch; subscriber dispatch keys on the oneof tag, so the
// transport adapter just serialises + delivers without
// inspecting the contents.
//
// Channel selects the transport's stream/subject; topic
// becomes the filter key the subscriber-side glob matches
// against.
type Dispatcher interface {
	Dispatch(ctx context.Context, channel, topic string, envelope proto.Message) error
}
