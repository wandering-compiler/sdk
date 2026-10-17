package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventlog"
	"github.com/wandering-compiler/sdk/go/lib/redact"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

func init() {
	redact.Register(map[string]redact.Plan{"google.protobuf.FieldDescriptorProto": {}})
}

// A tool call is recorded — a refused one too — with its request and gRPC
// code; a tool the stream does not know records nothing.
func TestCallUnary_RecordsTheCall(t *testing.T) {
	var mu sync.Mutex
	var recs [][]byte
	r, err := eventlog.NewRecorder(context.Background(), eventlog.RecorderConfig{
		Domain:   "tasks",
		Surfaces: map[w17pb.RequestApi]eventlog.Purposes{w17pb.RequestApi_REQUEST_API_MCP: {Audit: true}},
		Fail:     func(eventlog.Failure) {},
		Sink: func(w17pb.RequestApi) func(context.Context, *eventlog.Batch) error {
			return func(_ context.Context, b *eventlog.Batch) error {
				mu.Lock()
				defer mu.Unlock()
				for i := 0; i < b.Len(); i++ {
					recs = append(recs, append([]byte(nil), b.Record(i)...))
				}
				return nil
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("t", "1", nil)
	s.SetToolPerm("danger", 9)
	s.SetPermResolver(func(context.Context, ConnectionInfo) ([]int32, error) { return []int32{1}, nil })
	s.SetRequestEvents(r.Surface(w17pb.RequestApi_REQUEST_API_MCP), map[string]*eventlog.Method{
		"echo":   {FullName: "/acme.Tasks/Echo", RequestTag: 3},
		"danger": {FullName: "/acme.Tasks/Danger", RequestTag: 3},
	})
	call := func(name string, args map[string]any) {
		req := mcp.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		_, _ = s.CallUnary(context.Background(), req, &descriptorpb.FieldDescriptorProto{},
			func(context.Context, proto.Message) (proto.Message, error) {
				return &wrapperspb.StringValue{Value: "ok"}, nil
			})
	}
	call("echo", map[string]any{"name": "hello"})
	call("danger", nil)
	call("unknown", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("%d records, want echo and the refused danger", len(recs))
	}
	got := map[string]int32{}
	for _, b := range recs {
		var meta w17pb.RequestEventMeta
		var req []byte
		for len(b) > 0 {
			num, _, n := protowire.ConsumeTag(b)
			b = b[n:]
			v, n := protowire.ConsumeBytes(b)
			b = b[n:]
			switch num {
			case eventlog.EnvelopeMetaField:
				_ = proto.Unmarshal(v, &meta)
			case 3:
				req = v
			}
		}
		got[meta.GetMethod()] = meta.GetGrpcCode()
		if meta.GetMethod() == "/acme.Tasks/Echo" {
			var fd descriptorpb.FieldDescriptorProto
			if err := proto.Unmarshal(req, &fd); err != nil || fd.GetName() != "hello" {
				t.Fatalf("echo request %q %v", fd.GetName(), err)
			}
		}
	}
	if got["/acme.Tasks/Echo"] != 0 || got["/acme.Tasks/Danger"] != 7 /* PermissionDenied */ {
		t.Fatalf("codes %v", got)
	}
}
