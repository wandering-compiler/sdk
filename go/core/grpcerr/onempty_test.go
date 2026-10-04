package grpcerr_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

func errorDetail(t *testing.T, err error) (*status.Status, *w17pb.ErrorDetail) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	for _, d := range st.Details() {
		if ed, isED := d.(*w17pb.ErrorDetail); isED {
			return st, ed
		}
	}
	t.Fatalf("no ErrorDetail attached to %v", err)
	return nil, nil
}

// A missing row is answered with the author's refusal, and every part of it
// reaches its reader: the code, the detail code a client branches on, the field
// a form highlights, the sentence with its hole filled — and, for the operator,
// WHICH op found nothing.
func TestOnEmpty_AMissingRowIsTheAuthorsRefusal(t *testing.T) {
	err := grpcerr.OnEmpty(context.Background(), "AuthMutation.UpdateOrgMembership",
		fmt.Errorf("scan: %w", sql.ErrNoRows), nil, grpcerr.DialectPostgres,
		codes.InvalidArgument, "r", "UNKNOWN_ROLE", "role",
		"There is no organization role named {role}.", map[string]string{"role": "admni"})

	st, detail := errorDetail(t, err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", st.Code())
	}
	if !strings.Contains(st.Message(), `op "r" found no row`) {
		t.Errorf("operator message does not name the op: %q", st.Message())
	}
	if detail.GetCode() != "UNKNOWN_ROLE" || detail.GetField() != "role" {
		t.Errorf("detail = %v, want code UNKNOWN_ROLE on field role", detail)
	}
	if detail.GetMessage() != "There is no organization role named admni." {
		t.Errorf("detail message = %q", detail.GetMessage())
	}
}

// Every other error keeps the classification Wrap gives it. A refusal written
// for "no such row" must not relabel a cancelled request as a typo.
func TestOnEmpty_AnyOtherErrorIsWrapsAnswer(t *testing.T) {
	err := grpcerr.OnEmpty(context.Background(), "M", context.Canceled, nil, grpcerr.DialectPostgres,
		codes.InvalidArgument, "r", "UNKNOWN_ROLE", "role", "There is no organization role named {role}.", nil)

	st, detail := errorDetail(t, err)
	if st.Code() != codes.Canceled {
		t.Errorf("code = %s, want Canceled", st.Code())
	}
	if detail.GetCode() == "UNKNOWN_ROLE" {
		t.Errorf("a cancellation was answered with the op's refusal: %v", detail)
	}
}
