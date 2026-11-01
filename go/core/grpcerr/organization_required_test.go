package grpcerr

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The unnamed-organization refusal is its own CODE (not PERMISSION_DENIED), its
// own detail code and its own sentence — the three things that let a product
// tell "ask which organization" from "no access".
func TestOrganizationRequired_IsDistinctFromEveryDenial(t *testing.T) {
	err := OrganizationRequired(context.Background(), "app.Svc.M", "organization required: missing permission app.Svc.M")
	st := status.Convert(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition", st.Code())
	}
	if !strings.HasPrefix(st.Message(), "app.Svc.M: ") {
		t.Errorf("operator message = %q, want the method prefix", st.Message())
	}
	var detail *w17pb.ErrorDetail
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			detail = ed
		}
	}
	if detail == nil {
		t.Fatal("no ErrorDetail — the caller would get the generic FAILED_PRECONDITION sentence")
	}
	if detail.GetCode() != CodeOrganizationRequired {
		t.Errorf("detail code = %q, want %q", detail.GetCode(), CodeOrganizationRequired)
	}
	if detail.GetMessage() != MsgidOrganizationRequired {
		t.Errorf("detail message = %q, want the msgid", detail.GetMessage())
	}
	if !strings.Contains(MsgidOrganizationRequired, "W17-Org") {
		t.Error("the sentence does not name the header the caller has to send")
	}
	for _, c := range []string{CodeMissingPermission, CodeNoPermissions, CodeNoPrincipal} {
		if c == CodeOrganizationRequired {
			t.Errorf("ORGANIZATION_REQUIRED collides with %s", c)
		}
	}
}

func TestVocabulary_CarriesTheOrganizationRequiredSentence(t *testing.T) {
	if !slices.Contains(Vocabulary(), MsgidOrganizationRequired) {
		t.Error("Vocabulary() does not carry MsgidOrganizationRequired — no catalogue would ever translate it")
	}
}
