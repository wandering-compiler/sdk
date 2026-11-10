package grpcerr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// headerStream records what a server sets as response headers.
type headerStream struct {
	grpc.ServerTransportStream
	md metadata.MD
}

func (h *headerStream) Method() string { return "/svc/M" }
func (h *headerStream) SetHeader(md metadata.MD) error {
	h.md = metadata.Join(h.md, md)
	return nil
}
func (h *headerStream) SendHeader(md metadata.MD) error { return h.SetHeader(md) }
func (h *headerStream) SetTrailer(metadata.MD) error    { return nil }

// Only an UNAVAILABLE that is not marked replay-safe is marked answered; the
// replay-safe mark keeps the status as it is.
func TestMarkAnswered_OnlyAHandlersUnavailable(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "upstream down")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"unavailable":             {unavailable, true},
		"wrapped unavailable":     {fmt.Errorf("step two: %w", unavailable), true},
		"replay-safe unavailable": {ReplaySafe(unavailable), false},
		"aborted":                 {status.Error(codes.Aborted, "conflict"), false},
		"plain error":             {errors.New("boom"), false},
		"nil":                     {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			hs := &headerStream{}
			ctx := grpc.NewContextWithServerTransportStream(context.Background(), hs)
			MarkAnswered(ctx, tc.err)
			if got := len(hs.md.Get(AnsweredHeader)) > 0; got != tc.want {
				t.Fatalf("marked = %v, want %v (headers %v)", got, tc.want, hs.md)
			}
		})
	}
	st, _ := status.FromError(ReplaySafe(unavailable))
	if st.Code() != codes.Unavailable || st.Message() != "upstream down" {
		t.Fatalf("ReplaySafe changed the status: %v %q", st.Code(), st.Message())
	}
	if ReplaySafe(nil) != nil {
		t.Fatal("ReplaySafe(nil) != nil")
	}
	// Without a server transport (an in-process call) there is nothing to set.
	MarkAnswered(context.Background(), unavailable)
}
