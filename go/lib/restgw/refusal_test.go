package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// WriteBadInput keeps the FIELD and drops the parser's prose.
//
// The two halves are asserted separately because each alone is satisfiable by
// the wrong implementation: "no strconv text in the body" also holds for a body
// that says nothing at all, and "the field is named" also holds for the old
// message that named it next to the Go error.
func TestWriteBadInput(t *testing.T) {
	cause := errors.New(`strconv.ParseInt: parsing "abc": invalid syntax`)

	rec := httptest.NewRecorder()
	logged := captureLog(t, func() {
		restgw.WriteBadInput(context.Background(), rec, "path param", "user_id", cause)
	})
	body := rec.Body.String()

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(body, `"field":"user_id"`) {
		t.Errorf("the caller cannot tell which input was refused: %s", body)
	}
	if !strings.Contains(body, restgw.MsgidMalformedValue) {
		t.Errorf("body lacks the sentence for a malformed value: %s", body)
	}
	for _, leaked := range []string{"strconv", "invalid syntax", "ParseInt"} {
		if strings.Contains(body, leaked) {
			t.Errorf("body carries the parser's prose %q: %s", leaked, body)
		}
	}
	if !strings.Contains(logged, "invalid syntax") || !strings.Contains(logged, "user_id") {
		t.Errorf("the operator lost the parser's own words: %q", logged)
	}
	// The binding, not just the name: one field name can be bound from the
	// path, the query string and a form part, and only this says which.
	if !strings.Contains(logged, "path param user_id") {
		t.Errorf("the operator cannot tell where the value came from: %q", logged)
	}
}

// A refusal is logged whether or not W17_OBSERVX_DEBUG is set.
//
// This is the assertion that would have caught the fix-by-deletion: routing the
// operator's copy to observx.ReportEvent compiles, closes the leak, passes every
// body assertion above, and tells the operator nothing in a default deployment.
func TestWriteRefusal_ReachesTheOperatorWithNoKnobSet(t *testing.T) {
	t.Setenv("W17_OBSERVX_DEBUG", "")

	logged := captureLog(t, func() {
		restgw.WriteRefusal(context.Background(), httptest.NewRecorder(),
			codes.PermissionDenied, "forbidden: missing permission tasks.TaskQuery.ListTasksPaged")
	})
	if !strings.Contains(logged, "tasks.TaskQuery.ListTasksPaged") {
		t.Errorf("the refusal reached neither the caller nor the operator: %q", logged)
	}
}

// The gateway's own refusals and a backend's render the same envelope, so a
// client needs one branch, not two.
func TestWriteRefusal_MatchesTheBackendPathsEnvelope(t *testing.T) {
	own := httptest.NewRecorder()
	restgw.WriteRefusal(context.Background(), own, codes.PermissionDenied, "operator detail")

	backend := httptest.NewRecorder()
	restgw.WriteGRPCError(backend, grpcstatus.Error(codes.PermissionDenied, "TaskQuery.List: denied"))

	if own.Body.String() != backend.Body.String() {
		t.Errorf("two shapes for one refusal:\n gateway: %s\n backend: %s", own.Body, backend.Body)
	}
	if own.Code != backend.Code {
		t.Errorf("status %d vs %d", own.Code, backend.Code)
	}
}
