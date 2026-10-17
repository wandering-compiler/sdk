package grpcerr

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/redact"
)

// The interceptor records the request, so an error reported while serving it
// carries it — redacted (field 7 is planned out and holds the decoy).
func TestRecoverUnary_ErrorCarriesTheRequest(t *testing.T) {
	redact.Register(map[string]redact.Plan{"google.protobuf.FieldDescriptorProto": {Skip: []protowire.Number{7}}})
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	req := &descriptorpb.FieldDescriptorProto{Name: proto.String("plain-value"), DefaultValue: proto.String("DECOY")}
	info := &grpc.UnaryServerInfo{FullMethod: "/app.Storage/Create"}
	_, _ = RecoverUnaryInterceptor(context.Background(), req, info, func(ctx context.Context, _ any) (any, error) {
		observx.ReportError(ctx, context.DeadlineExceeded)
		return nil, nil
	})
	line := buf.String()
	if !strings.Contains(line, "method=/app.Storage/Create") || !strings.Contains(line, "plain-value") {
		t.Fatalf("the reported error lacks the request: %q", line)
	}
	if strings.Contains(line, "DECOY") {
		t.Fatalf("a sensitive value reached the report: %q", line)
	}
}
