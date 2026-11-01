package restgw_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	"github.com/wandering-compiler/sdk/go/lib/acllock"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

type deniedEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"details"`
	} `json:"error"`
}

func writeDenied(t *testing.T, scopes map[string]string) (*httptest.ResponseRecorder, deniedEnvelope) {
	t.Helper()
	rec := httptest.NewRecorder()
	restgw.WriteDeniedCtx(context.Background(), rec, "scout.HouseQuery.ListSupportedAreas", 7, scopes)
	var env deniedEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v — body %s", err, rec.Body.String())
	}
	return rec, env
}

// A caller who holds the permission inside an organization the request does
// not name is told to name one — not "no access". The status, the canonical
// code and the detail code all differ from MISSING_PERMISSION, so a client can
// tell the two apart on any of them.
func TestWriteDeniedCtx_UnnamedOrganizationIsOrganizationRequired(t *testing.T) {
	rec, env := writeDenied(t, map[string]string{acllock.OrgPermissionsScope: "3,7"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (FAILED_PRECONDITION)", rec.Code)
	}
	if env.Error.Code != "FAILED_PRECONDITION" {
		t.Errorf("code = %q, want FAILED_PRECONDITION", env.Error.Code)
	}
	if len(env.Error.Details) != 1 || env.Error.Details[0].Code != "ORGANIZATION_REQUIRED" {
		t.Fatalf("details = %+v, want one ORGANIZATION_REQUIRED", env.Error.Details)
	}
	if !strings.Contains(env.Error.Message, "W17-Org") || env.Error.Details[0].Message != env.Error.Message {
		t.Errorf("message = %q / detail %q, want the sentence naming W17-Org in both", env.Error.Message, env.Error.Details[0].Message)
	}
	if strings.Contains(rec.Body.String(), "ListSupportedAreas") {
		t.Errorf("the permission name reached the caller: %s", rec.Body.String())
	}
}

// Everything else stays exactly the 403 it was.
func TestWriteDeniedCtx_OtherwiseMissingPermission(t *testing.T) {
	for name, scopes := range map[string]map[string]string{
		"no scopes":            nil,
		"no hint":              {"org_id": "o1"},
		"hint without this id": {acllock.OrgPermissionsScope: "3,70"},
	} {
		rec, env := writeDenied(t, scopes)
		if rec.Code != http.StatusForbidden || env.Error.Code != "PERMISSION_DENIED" {
			t.Errorf("%s: %d %s, want 403 PERMISSION_DENIED", name, rec.Code, env.Error.Code)
		}
		if len(env.Error.Details) != 1 || env.Error.Details[0].Code != "MISSING_PERMISSION" {
			t.Errorf("%s: details = %+v, want MISSING_PERMISSION", name, env.Error.Details)
		}
	}
}

// Authenticate's refusal of an organization the caller is not in (the auth
// plugin's ORG_NOT_MEMBER) is PERMISSION_DENIED: the credential was accepted.
// The auth gate renders it 403 with the code intact — never the 401 that
// tells a client its session is gone.
func TestWriteAuthError_OrgNotMemberIsForbiddenNotUnauthorized(t *testing.T) {
	st, err := status.New(codes.PermissionDenied, "the caller is not a member of the organization this request names").
		WithDetails(protoadapt.MessageV1Of(&w17pb.ErrorDetail{Code: "ORG_NOT_MEMBER", Message: "You are not a member of the organization this request names."}))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	restgw.WriteAuthError(rec, st.Err())
	var env deniedEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v — body %s", err, rec.Body.String())
	}
	if rec.Code != http.StatusForbidden || env.Error.Code != "PERMISSION_DENIED" {
		t.Errorf("%d %s, want 403 PERMISSION_DENIED", rec.Code, env.Error.Code)
	}
	if len(env.Error.Details) != 1 || env.Error.Details[0].Code != "ORG_NOT_MEMBER" {
		t.Errorf("details = %+v, want ORG_NOT_MEMBER", env.Error.Details)
	}
}
