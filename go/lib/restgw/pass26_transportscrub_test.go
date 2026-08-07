package restgw_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// T2-6 pass #5 D3 — restgw-sec-3 says a transport error's text can carry
// internal topology and must not reach the client, but it only guarded the
// NON-status branch. grpc-go reports a failed dial as a STATUS error
// (picker_wrapper.go wraps the balancer's last error in
// status.Error(codes.Unavailable, err.Error())), so real transport failures
// took the other branch and their message went out verbatim — backend host
// and port included.

func bodyOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not an error envelope: %v (%s)", err, rec.Body)
	}
	return env.Error
}

func TestWriteGRPCError_DialFailureDoesNotLeakBackendAddress(t *testing.T) {
	// Verbatim shape of what grpc-go produces when the backend is down.
	err := status.Error(codes.Unavailable,
		`connection error: desc = "transport: Error while dialing: dial tcp app-storage:9090: connect: connection refused"`)

	rec := httptest.NewRecorder()
	restgw.WriteGRPCError(rec, err)

	body := rec.Body.String()
	for _, leak := range []string{"app-storage", "9090", "dial tcp", "connection refused"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks internal topology (%q): %s", leak, body)
		}
	}
	// The code still has to reach the client — it drives retry behaviour.
	if got := bodyOf(t, rec)["code"]; got != "UNAVAILABLE" {
		t.Errorf("code = %v, want UNAVAILABLE", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// The balancer's error also rides out on DeadlineExceeded when a deadline
// expires during an outage ("latest balancer error: ...").
func TestWriteGRPCError_DeadlineCarryingBalancerErrorIsScrubbed(t *testing.T) {
	err := status.Error(codes.DeadlineExceeded,
		`context deadline exceeded: latest balancer error: connection error: desc = "transport: Error while dialing: dial tcp 10.4.2.7:50051: i/o timeout"`)

	rec := httptest.NewRecorder()
	restgw.WriteGRPCError(rec, err)

	for _, leak := range []string{"10.4.2.7", "50051", "dial tcp", "balancer"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response leaks internal topology (%q): %s", leak, rec.Body)
		}
	}
}

// Our own handlers' messages come from grpcerr.Wrap and are deliberately
// authored — scrubbing must not touch them, or forms lose their prose.
func TestWriteGRPCError_HandlerAuthoredMessagesSurvive(t *testing.T) {
	cases := []struct {
		code codes.Code
		msg  string
	}{
		{codes.NotFound, "TaskQuery.GetTask: not found"},
		{codes.InvalidArgument, "TaskMutation.CreateTask: unique violation"},
		{codes.FailedPrecondition, "BillingService.Charge: subscription is not active"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		restgw.WriteGRPCError(rec, status.Error(tc.code, tc.msg))
		if got := bodyOf(t, rec)["message"]; got != tc.msg {
			t.Errorf("%v: message = %v, want %q (authored prose must survive)", tc.code, got, tc.msg)
		}
	}
}

// An Unavailable a handler raised itself still gets scrubbed: we cannot tell
// it apart from a transport one, and Unavailable carries no prose a client
// acts on beyond the code.
func TestWriteGRPCError_UnavailableIsGenericised(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteGRPCError(rec, status.Error(codes.Unavailable, "upstream pool exhausted at 10.0.0.4"))
	if strings.Contains(rec.Body.String(), "10.0.0.4") {
		t.Errorf("Unavailable message must be genericised: %s", rec.Body)
	}
}
