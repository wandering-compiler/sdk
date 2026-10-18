package eventlog

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// HeaderOrgs is the NATS header naming the organizations a batch's records
// belong to ([Batch.Tags], comma-separated; "*" when there were more than
// [MaxBatchTags]), so a tenant's consumer skips a batch without decoding it.
const HeaderOrgs = "W17-Orgs"

// natsHeaderRoom is what a batch leaves of the server's max_payload for the
// headers (the message id, the organizations) and the subject.
const natsHeaderRoom = 4 << 10

// NATSBatchBytes is the [Config.BatchBytes] for a connection: the server's
// max_payload (read at connect, not raised) less the room headers take.
func NATSBatchBytes(nc *nats.Conn) int {
	return int(nc.MaxPayload()) - natsHeaderRoom
}

// NATSSink publishes each batch to JetStream on one subject and waits for the
// stream's acknowledgement — a batch counts as delivered only once it is
// stored. The batch id rides `Nats-Msg-Id`, so JetStream's duplicate window
// drops a batch sent twice.
type NATSSink struct {
	JS      jetstream.JetStream
	Subject string
	// Timeout bounds one publish and its ack. Default 5s.
	Timeout time.Duration
	// Ensure, when set, creates the stream; the sink calls it when a
	// publish finds no stream (it could not be created at start, or was
	// deleted) and publishes once more.
	Ensure func(context.Context) error
}

// Sink is a [Config.Sink].
func (s *NATSSink) Sink(ctx context.Context, b *Batch) error {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	msg := &nats.Msg{Subject: s.Subject, Data: b.Wire, Header: nats.Header{}}
	msg.Header.Set(jetstream.MsgIDHeader, b.ID.String())
	switch {
	case b.TagsOverflow:
		msg.Header.Set(HeaderOrgs, "*")
	case len(b.Tags) > 0:
		msg.Header.Set(HeaderOrgs, strings.Join(b.Tags, ","))
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ack, err := s.JS.PublishMsg(pctx, msg)
	if err != nil && s.Ensure != nil && errors.Is(err, jetstream.ErrNoStreamResponse) {
		if eerr := s.Ensure(pctx); eerr != nil {
			return fmt.Errorf("eventlog: publish to %s: no stream, and creating it failed: %w", s.Subject, eerr)
		}
		ack, err = s.JS.PublishMsg(pctx, msg)
	}
	if err != nil {
		return fmt.Errorf("eventlog: publish to %s: %w", s.Subject, err)
	}
	if ack == nil || ack.Stream == "" {
		return errors.New("eventlog: publish to " + s.Subject + ": no stream acknowledged it")
	}
	return nil
}

// StreamLimits are the request stream's bounds: a stream without them fills
// the disk.
type StreamLimits struct {
	MaxAge time.Duration // default 7 days
	// MaxBytes caps the stream (default 10 GiB). JetStream reserves it out of
	// the server's storage when the stream is created; a server without room
	// for it gets the largest halving of it that fits, down to
	// [MinStreamBytes], said in the log.
	MaxBytes int64
	Replicas int // default 1; 3 for a highly available cluster
	// Duplicates is the window in which a re-sent batch id is dropped.
	// Default 2 minutes.
	Duplicates time.Duration
}

// MinStreamBytes is the smallest cap [EnsureStream] halves a stream's
// MaxBytes down to when the server has no room for it.
const MinStreamBytes = 64 << 20

// jsInsufficientResources is JetStream's error code for a stream whose
// MaxBytes the server cannot reserve.
const jsInsufficientResources = 10047

// EnsureStream creates the JetStream stream that stores the request records,
// over subjects (e.g. "<prefix>.>"), with its limits — when it does not exist
// yet. An existing stream is left as it is: whoever created it, or an
// operator who changed its limits since, decided them, and a binary booting
// with its defaults must not undo that.
func EnsureStream(ctx context.Context, js jetstream.JetStream, name string, subjects []string, l StreamLimits) error {
	if _, err := js.Stream(ctx, name); err == nil {
		return nil
	} else if !errors.Is(err, jetstream.ErrStreamNotFound) {
		return fmt.Errorf("eventlog: ensure stream %s: %w", name, err)
	}
	if l.MaxAge <= 0 {
		l.MaxAge = 7 * 24 * time.Hour
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 10 << 30
	}
	if l.Replicas <= 0 {
		l.Replicas = 1
	}
	if l.Duplicates <= 0 {
		l.Duplicates = 2 * time.Minute
	}
	want := l.MaxBytes
	for {
		_, err := js.CreateStream(ctx, jetstream.StreamConfig{
			Name:       name,
			Subjects:   subjects,
			Retention:  jetstream.LimitsPolicy,
			Discard:    jetstream.DiscardOld,
			MaxAge:     l.MaxAge,
			MaxBytes:   l.MaxBytes,
			MaxMsgs:    -1,
			Replicas:   l.Replicas,
			Duplicates: l.Duplicates,
		})
		var apiErr *jetstream.APIError
		switch {
		case err == nil:
			if l.MaxBytes < want {
				log.Printf("eventlog: stream %s capped at %d MiB: the JetStream server has no room for the %d MiB asked — raise its storage (max_file_store) and the stream's max_bytes", name, l.MaxBytes>>20, want>>20)
			}
			return nil
		case errors.Is(err, jetstream.ErrStreamNameAlreadyInUse):
			return nil // created meanwhile, by another binary
		case errors.As(err, &apiErr) && apiErr.ErrorCode == jsInsufficientResources && l.MaxBytes/2 >= MinStreamBytes:
			l.MaxBytes /= 2
		default:
			return fmt.Errorf("eventlog: ensure stream %s (max_bytes %d MiB): %w", name, l.MaxBytes>>20, err)
		}
	}
}
