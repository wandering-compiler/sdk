package restgw_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// B25-restgw-1: WriteGRPCError must forward a backend status's *w17.ErrorDetail
// field violations into the REST error envelope. grpcerr.Wrap attaches them for
// DB constraint violations (e.g. a UNIQUE email) that gateway pre-flight can't
// catch; dropping them leaves the client with an empty `details` array, unable
// to highlight the offending field.
func TestWriteGRPCError_ForwardsFieldViolationDetails(t *testing.T) {
	st, err := status.New(codes.AlreadyExists, "email already in use").WithDetails(
		protoadapt.MessageV1Of(&w17pb.ErrorDetail{Field: "email", Code: "UNIQUE_VIOLATION", Message: "already taken"}),
	)
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}

	rec := httptest.NewRecorder()
	restgw.WriteGRPCError(rec, st.Err())

	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Field   string `json:"field"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"details"`
		} `json:"error"`
	}
	if e := json.Unmarshal(rec.Body.Bytes(), &env); e != nil {
		t.Fatalf("unmarshal body: %v\n%s", e, rec.Body.String())
	}
	if len(env.Error.Details) != 1 {
		t.Fatalf("details length = %d, want 1 (field violation dropped)\n%s", len(env.Error.Details), rec.Body.String())
	}
	d := env.Error.Details[0]
	if d.Field != "email" || d.Code != "UNIQUE_VIOLATION" {
		t.Errorf("detail = %+v, want {email, UNIQUE_VIOLATION}", d)
	}
}
