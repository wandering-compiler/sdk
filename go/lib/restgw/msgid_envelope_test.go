package restgw

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
)

// A backend's detail reaches the JSON envelope with its catalog key and
// params, so a client re-translates it.
func TestEnvelopeCarriesMsgidAndParams(t *testing.T) {
	err := grpcerr.OnPresent(context.Background(), "svc.M", codes.FailedPrecondition, "lock", "PERIOD_LOCKED", "", "The period {period} is locked.", map[string]string{"period": "2026-11"})
	rec := httptest.NewRecorder()
	WriteGRPCErrorCtx(context.Background(), rec, err)
	var env struct {
		Error struct {
			Details []FieldError `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Error.Details) != 1 {
		t.Fatalf("details = %s", rec.Body.String())
	}
	d := env.Error.Details[0]
	if d.Msgid != "The period {period} is locked." || d.Params["period"] != "2026-11" {
		t.Errorf("detail = %+v", d)
	}
}
