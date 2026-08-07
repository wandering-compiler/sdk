package inprocgrpc_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// A goroutine the handler spawned must not kill the process by sending late.
//
// The end of a stream used to be signalled by CLOSING the server→client
// channel. Closing a channel a producer may still be sending on is a panic,
// and it happens on a goroutine the handler spawned — where no recover can
// catch it. Over the wire that same late send is an error on one RPC; in a
// composed binary, where every in-process call rides this transport, it took
// the whole PROCESS down.
//
// Reordering the close and the cancel does not fix it: a sender parked in the
// select picks at random between a ready Done() and a closed send channel.
// The end of the stream needs a channel of its own.
//
// The corpus already contains the shape — a handler that spawns a worker and
// streams from it — deployed today only over the wire.
func TestServerStream_LateSendFromASpawnedGoroutineDoesNotPanic(t *testing.T) {
	sent := make(chan error, 1)
	handlerReturned := make(chan struct{})

	srv := inprocgrpc.New()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "probe.Streamer",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Live",
			ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				// A worker that outlives the handler — the shape under test.
				// It PARKS in SendMsg (the channel is unbuffered and nobody
				// is receiving yet), which is the deterministic form: the
				// stream then ends underneath a sender that is already
				// inside the send.
				started := make(chan struct{})
				go func() {
					close(started)
					sent <- stream.SendMsg(wrapperspb.String("late"))
				}()
				<-started
				// Give the worker time to reach the send before returning.
				time.Sleep(50 * time.Millisecond)
				return nil
			},
		}},
	}, struct{}{})

	stream, err := srv.NewStream(context.Background(),
		&grpc.StreamDesc{StreamName: "Live", ServerStreams: true}, "/probe.Streamer/Live")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	// Deliberately do NOT receive: the worker must still be PARKED inside
	// SendMsg when the handler returns and the stream ends underneath it.
	// A client that drains lets the send complete and the window closes.
	_ = stream
	_ = handlerReturned

	select {
	case err := <-sent:
		if err == nil {
			t.Error("a send after the handler returned reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the late send never returned — it is parked on a channel nobody will read")
	}
}
