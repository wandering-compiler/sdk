package eventlog

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type reusingStream struct {
	grpc.ServerStream
	msgs []string
}

func (s *reusingStream) Context() context.Context    { return context.Background() }
func (s *reusingStream) SetHeader(metadata.MD) error { return nil }
func (s *reusingStream) RecvMsg(m any) error {
	v := m.(*wrapperspb.StringValue)
	v.Value, s.msgs = s.msgs[0], s.msgs[1:]
	return nil
}

// A record is encoded after the call, on the pipeline's goroutine, so it
// MUST own the request it records. A streaming handler that reuses its
// receive buffer (`var in pb.Msg; stream.RecvMsg(&in)`, the idiomatic loop)
// would otherwise have its LAST message recorded as the stream's request,
// and a handler that changes a decoded request after the surface recorded
// it would change the record.
func TestRecord_KeepsItsOwnCopyOfTheRequest(t *testing.T) {
	rec := &Record{}
	cs := &countedStream{ServerStream: &reusingStream{msgs: []string{"first", "second"}}, rec: rec}
	var in wrapperspb.StringValue
	for i := 0; i < 2; i++ {
		if err := cs.RecvMsg(&in); err != nil {
			t.Fatal(err)
		}
	}
	if got := rec.req.(*wrapperspb.StringValue).GetValue(); got != "first" {
		t.Fatalf("stream record request = %q, want the first message", got)
	}

	rec = &Record{}
	req := wrapperspb.String("as decoded")
	rec.SetRequest(req)
	req.Value = "changed by the handler"
	if got := rec.req.(*wrapperspb.StringValue).GetValue(); got != "as decoded" {
		t.Fatalf("record request = %q, want the request as decoded", got)
	}
	if proto.Equal(rec.req, req) {
		t.Fatal("the record shares the handler's message")
	}
}
