package eventlog

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// fakeStream hands out recv messages until they run out, and accepts every
// send but the ones failSend names.
type fakeStream struct {
	grpc.ServerStream
	recv     int
	failSend int
	sends    int
}

func (s *fakeStream) Context() context.Context { return context.Background() }

func (s *fakeStream) RecvMsg(m any) error {
	if s.recv == 0 {
		return io.EOF
	}
	s.recv--
	return nil
}

func (s *fakeStream) SendMsg(any) error {
	s.sends++
	if s.sends == s.failSend {
		return errors.New("client gone")
	}
	return nil
}

// A streaming RPC is one record: every message counted (a failed send is
// not one), the first message received kept as the request, no response,
// the stream's code.
func TestRecord_StreamCountsItsMessages(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_RPC: {Audit: true}})
	rec := r.Surface(w17pb.RequestApi_REQUEST_API_RPC).Start(testMethod)
	ss := rec.CountStream(&fakeStream{recv: 2, failSend: 4})
	for i := 0; i < 3; i++ {
		if err := ss.RecvMsg(sampleReq()); i < 2 && err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		_ = ss.SendMsg(sampleResp())
	}
	rec.FinishStream(context.Background(), status.Error(codes.Canceled, "client left"))
	closeRecorder(t, r)
	e := decodeEnvelope(t, c.records(w17pb.RequestApi_REQUEST_API_RPC)[0])
	if e.meta.GetMessagesSent() != 3 || e.meta.GetMessagesReceived() != 2 {
		t.Fatalf("sent %d received %d, want 3 and 2", e.meta.GetMessagesSent(), e.meta.GetMessagesReceived())
	}
	if e.req == nil || e.resp != nil {
		t.Fatalf("request %v response %v: want the first message and no response", e.req != nil, e.resp != nil)
	}
	if e.meta.GetGrpcCode() != int32(codes.Canceled) {
		t.Fatalf("grpc code %d", e.meta.GetGrpcCode())
	}
}

// A WebSocket counts from two goroutines (its pump and the response loop);
// a unary request counts nothing; a nil record ignores it all.
func TestRecord_MessageCountsFromTwoGoroutines(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	rec := s.Start(testMethod)
	var wg sync.WaitGroup
	for _, f := range []func(){rec.MessageSent, rec.MessageReceived} {
		wg.Add(1)
		go func(f func()) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				f()
			}
		}(f)
	}
	wg.Wait()
	rec.Finish(context.Background(), 200)
	s.Start(testMethod).Finish(context.Background(), 200)
	var none *Record
	none.MessageSent()
	none.MessageReceived()
	none.FinishStream(context.Background(), nil)
	if none.CountStream(nil) != nil {
		t.Fatal("a nil record wraps the stream")
	}
	closeRecorder(t, r)
	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 2 {
		t.Fatalf("%d records", len(recs))
	}
	got := map[[2]int64]bool{}
	for _, b := range recs {
		m := decodeEnvelope(t, b).meta
		got[[2]int64{m.GetMessagesSent(), m.GetMessagesReceived()}] = true
	}
	if !got[[2]int64{100, 100}] || !got[[2]int64{0, 0}] {
		t.Fatalf("counts %v, want the stream's 100/100 and the unary request's 0/0", got)
	}
}
