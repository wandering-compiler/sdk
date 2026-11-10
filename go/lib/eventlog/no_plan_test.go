package eventlog

import (
	"context"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// A request whose type has no redaction plan is not recorded at all, meta
// included: fail closed, nothing is stored that a plan does not cover. The
// record takes the failure path, without an encoding.
func TestRecorder_ARequestWithoutARedactionPlanIsNotRecorded(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	rec := r.Surface(w17pb.RequestApi_REQUEST_API_REST).Start(testMethod)
	rec.SetRequest(wrapperspb.String("no plan for this type"))
	rec.Finish(context.Background(), 200)
	closeRecorder(t, r)
	if n := len(c.records(w17pb.RequestApi_REQUEST_API_REST)); n != 0 {
		t.Fatalf("%d records stored for a request without a redaction plan", n)
	}
	if len(c.fails) != 1 || c.fails[0].Count != 1 || len(c.fails[0].Records) != 0 || c.fails[0].Err == nil {
		t.Fatalf("failures = %+v; want one, with its reason and no encoding", c.fails)
	}
}
