package grpcerr_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The detail a person reads carries the catalog key it was translated from
// and its params, so a client holding the catalogs re-translates it.
func TestForUserCarriesMsgidAndParams(t *testing.T) {
	err := grpcerr.OnPresent(context.Background(), "svc.M", codes.FailedPrecondition, "lock", "PERIOD_LOCKED", "", "The period {period} is locked.", map[string]string{"period": "2026-11"})
	st, _ := status.FromError(err)
	var d *w17pb.ErrorDetail
	for _, x := range st.Details() {
		if ed, ok := x.(*w17pb.ErrorDetail); ok {
			d = ed
		}
	}
	if d == nil {
		t.Fatal("no detail")
	}
	if d.GetMsgid() != "The period {period} is locked." || d.GetParams()["period"] != "2026-11" || d.GetMessage() != "The period 2026-11 is locked." {
		t.Errorf("detail = %+v", d)
	}
}
