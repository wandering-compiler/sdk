package grpcerr_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/protobuf/proto"
)

// ForUserWith exists for one reason: a sentence that must name a VALUE, with the
// value beside the msgid instead of inside it.
//
// A consumer found out why by doing the other thing — they made the value part of
// the key (`period 2027-03 is already locked`), which is a new key every month, so
// no catalogue entry can match and the caller would eventually see the key itself.
// Interpolation keeps one entry a translator can hold.
//
// This is the only test of that function, and it was added on review: the function
// shipped with none, which for an SDK API whose whole point is the substitution
// means the substitution was the untested part.
func TestForUserWith_InterpolatesIntoTheSentenceTheCallerSees(t *testing.T) {
	err := grpcerr.ForUserWith(context.Background(), codes.FailedPrecondition,
		"LedgerService.LockPeriod", "period 2027-03 is already locked",
		"PERIOD_LOCKED", "Accounting period {period} is already closed.",
		map[string]string{"period": "2027-03"})

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", st.Code())
	}
	// The operator keeps the developer's prose.
	if !strings.Contains(st.Message(), "period 2027-03 is already locked") {
		t.Errorf("operator message lost the cause: %q", st.Message())
	}

	var detail *w17pb.ErrorDetail
	for _, d := range st.Details() {
		if m, isDetail := d.(proto.Message); isDetail {
			if ed, isED := m.(*w17pb.ErrorDetail); isED {
				detail = ed
			}
		}
	}
	if detail == nil {
		t.Fatal("no ErrorDetail attached — the caller would get the code's generic sentence")
	}
	if detail.GetCode() != "PERIOD_LOCKED" {
		t.Errorf("detail code = %q, want PERIOD_LOCKED", detail.GetCode())
	}
	if detail.GetMessage() != "Accounting period 2027-03 is already closed." {
		t.Errorf("detail message = %q — the value did not reach the sentence", detail.GetMessage())
	}
	// The hole must be GONE. A missed substitution ships `{period}` to a person,
	// which is worse than the generic sentence it replaced.
	if strings.Contains(detail.GetMessage(), "{period}") {
		t.Errorf("the placeholder survived: %q", detail.GetMessage())
	}
}

// Nil params must not leave a hole open either — that is ForUser's path, and a
// caller reaching ForUserWith with nothing to fill should get the msgid as
// written rather than a crash or an empty sentence.
func TestForUserWith_NoParamsLeavesTheMsgidAsWritten(t *testing.T) {
	err := grpcerr.ForUserWith(context.Background(), codes.NotFound,
		"M", "dev", "C", "Nothing to interpolate here.", nil)
	st, _ := status.FromError(err)
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			if ed.GetMessage() != "Nothing to interpolate here." {
				t.Errorf("detail message = %q, want the msgid unchanged", ed.GetMessage())
			}
			return
		}
	}
	t.Fatal("no ErrorDetail attached")
}
