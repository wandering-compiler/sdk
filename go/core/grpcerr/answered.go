package grpcerr

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// AnsweredHeader is the response header a w17 server sends with an
// `UNAVAILABLE` its handler (or an interceptor in front of it) returned.
//
// The w17 clients retry `UNAVAILABLE` (grpcclient.DefaultServiceConfig), and
// grpc-go replays a retryable status only when the response was
// Trailers-Only — no headers before the status. A handler that returns an
// error without having sent headers produces exactly that, so nothing told a
// transport `UNAVAILABLE` (the request never reached a handler: no
// connection, a refused stream, a server going away) from one a handler
// returned after it had done work — a write committed, an upstream called —
// and the client ran the handler again, up to three times. A header makes
// the response not Trailers-Only, so the client returns the status to its
// caller instead; what a transport failure answers carries no header and is
// still replayed. The header's value carries nothing; its presence is the
// signal.
const AnsweredHeader = "w17-answered"

// replaySafeError is an error a handler returns for a call that did nothing: the
// client may send it again. It is its status, unchanged.
type replaySafeError struct{ err error }

func (r replaySafeError) Error() string { return r.err.Error() }
func (r replaySafeError) Unwrap() error { return r.err }

// GRPCStatus is the wrapped error's status, as it is: status.FromError reads
// it without rewriting the message.
func (r replaySafeError) GRPCStatus() *status.Status {
	st, _ := status.FromError(r.err)
	return st
}

// ReplaySafe marks err as the answer of a call that did nothing, so a w17
// client may send it again: an `UNAVAILABLE` returned with it goes out
// without [AnsweredHeader] and stays retryable. [Wrap] marks the one such
// answer it gives — a database that could not be reached at all, so no
// statement ran. Mark nothing a handler returns after it changed anything.
func ReplaySafe(err error) error {
	if err == nil {
		return nil
	}
	return replaySafeError{err: err}
}

// IsReplaySafe reports whether err was marked with [ReplaySafe].
func IsReplaySafe(err error) bool {
	var r replaySafeError
	return errors.As(err, &r)
}

// answered reports whether a call that ended with err must tell the client
// it was answered: an `UNAVAILABLE` not marked replay-safe.
func answered(err error) bool {
	return err != nil && status.Code(err) == codes.Unavailable && !IsReplaySafe(err)
}

var answeredMD = metadata.Pairs(AnsweredHeader, "1")

// MarkAnswered sets [AnsweredHeader] on a unary call's response when err is
// an `UNAVAILABLE` its handler returned (not marked [ReplaySafe]), so w17
// clients do not replay the call. ctx is the context the server handed the
// interceptor. Without a gRPC server transport (an in-process call), or once
// headers were sent, there is nothing to set and nothing to do: such a
// response is not replayed anyway.
func MarkAnswered(ctx context.Context, err error) {
	if answered(err) {
		_ = grpc.SetHeader(ctx, answeredMD)
	}
}

// MarkAnsweredStream is [MarkAnswered] for a streaming call.
func MarkAnsweredStream(ss grpc.ServerStream, err error) {
	if answered(err) {
		_ = ss.SetHeader(answeredMD)
	}
}
