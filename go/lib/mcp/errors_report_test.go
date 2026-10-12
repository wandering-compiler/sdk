package mcp

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// A backend refusal reaching a tool call is the caller's outcome, reported on
// the refusal channel like the REST surface reports it; only server faults go
// down the exception channel.
func TestCallUnary_BackendErrorsAreReportedByCode(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{status.Error(codes.NotFound, "Svc.Get: row 7 missing"), "refused:"},
		{status.Error(codes.InvalidArgument, "Svc.Put: bad"), "refused:"},
		{status.Error(codes.PermissionDenied, "Svc.Put: denied"), "refused:"},
		{status.Error(codes.Internal, "Svc.Put: boom"), "error:"},
		{errors.New("dial tcp 10.0.0.1:9090: refused"), "error:"},
	} {
		var buf bytes.Buffer
		orig := log.Writer()
		log.SetOutput(&buf)
		s := NewServer("x", "1", nil)
		_, _ = s.CallUnary(context.Background(), mcp.CallToolRequest{}, &emptypb.Empty{},
			func(context.Context, proto.Message) (proto.Message, error) { return nil, tc.err })
		log.SetOutput(orig)
		if got := buf.String(); !strings.Contains(got, tc.want) {
			t.Errorf("%v: logged %q, want a %q line", tc.err, got, tc.want)
		}
	}
}
