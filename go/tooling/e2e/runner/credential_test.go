package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The defect these cover, measured rather than imagined: a case that set
// `headers: {Authorization: Basic …}` was SILENTLY ignored — the caller
// skips that key so a header map cannot override the reserved token — so
// the step ran on the scenario's bearer session instead. The positive
// case passed (that session held the permission) and only the negative
// one failed, which is the sole reason it was caught at all.
//
// `credential:` is the explicit third state. Its whole value is that the
// bearer must NOT also be sent: two credentials on one request is a
// request no caller makes, and leaving the bearer on is what let the
// session answer for the credential under test.

func captureAuth(t *testing.T, token, credential string, headers map[string]string) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := &RESTCaller{BaseURL: srv.URL, Client: srv.Client()}
	ep := Endpoint{Ref: "m.S.M", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/x"}
	if _, err := c.Call(context.Background(), ep, nil, token, headers, credential, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	return got
}

func TestCredentialReplacesTheBearer(t *testing.T) {
	got := captureAuth(t, "the-session-token", "Basic ZGlyZWN0b3J5LW5vYm9keTp4", nil)
	if got != "Basic ZGlyZWN0b3J5LW5vYm9keTp4" {
		t.Fatalf("Authorization = %q, want the step's credential", got)
	}
	// The dangerous direction: a Bearer surviving alongside would let the
	// scenario's session answer for the credential under test, and a
	// negative case would then pass for the wrong reason.
	if got == "Bearer the-session-token" {
		t.Fatal("the reserved token was sent instead of the credential")
	}
}

func TestWithoutACredentialTheBearerStillGoes(t *testing.T) {
	got := captureAuth(t, "the-session-token", "", nil)
	if got != "Bearer the-session-token" {
		t.Fatalf("Authorization = %q, want the reserved bearer — the credential field must be additive", got)
	}
}

// The skip that caused the original defect STAYS. A header map is not a
// place to replace a credential: it reads like ordinary metadata, and
// that is exactly why it was silent.
func TestAHeaderMapStillCannotOverrideAuthorization(t *testing.T) {
	got := captureAuth(t, "the-session-token", "", map[string]string{"Authorization": "Basic c25lYWt5"})
	if got != "Bearer the-session-token" {
		t.Fatalf("Authorization = %q — a header map must not be able to replace the credential", got)
	}
}

// And a header map must not be able to undo an explicit credential
// either, for the same reason in the other direction.
func TestAHeaderMapCannotOverrideAnExplicitCredential(t *testing.T) {
	got := captureAuth(t, "", "Basic ZXhwbGljaXQ=", map[string]string{"Authorization": "Basic c25lYWt5"})
	if got != "Basic ZXhwbGljaXQ=" {
		t.Fatalf("Authorization = %q, want the explicit credential", got)
	}
}

// Neither present: no Authorization at all. An `exclude_auth` endpoint
// must not receive an empty header, which some servers treat differently
// from an absent one.
func TestNoTokenAndNoCredentialSendsNoAuthorizationHeader(t *testing.T) {
	if got := captureAuth(t, "", "", nil); got != "" {
		t.Fatalf("Authorization = %q, want no header", got)
	}
}

// recordingCaller keeps what the scenario layer actually handed the
// caller, which is the only place the token-suppression decision is
// visible. The REST caller's own tests above cannot see it: by the time
// the request is built the scenario has already chosen.
type recordingCaller struct {
	token      string
	credential string
}

func (r *recordingCaller) Call(_ context.Context, _ Endpoint, _ map[string]any, token string, _ map[string]string, credential string, _ []FilePart) (map[string]any, error) {
	r.token, r.credential = token, credential
	return map[string]any{}, nil
}

// The scenario layer must NOT resolve the reserved token for a step that
// names a credential. Without this, an auth-required endpoint would send
// both — and the session would answer for the credential under test,
// which is exactly how the first version of the ops-auth cases passed
// while proving nothing.
func TestScenarioDoesNotResolveTheReservedTokenForACredentialStep(t *testing.T) {
	rc := &recordingCaller{}
	scope := newScope()
	scope.Capture("auth.token", "the-session-token")

	s := Step{
		Endpoint:   Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/m", AuthRequired: true},
		Credential: "Basic ZXhwbGljaXQ=",
		Label:      "credential step",
	}
	if err := RunSteps(context.Background(), scope, []Step{s}, map[string]Caller{"rest": rc}); err != nil {
		t.Fatalf("RunSteps: %v", err)
	}
	if rc.token != "" {
		t.Errorf("the reserved token reached the caller (%q) alongside a credential — the session can then "+
			"answer for the credential under test", rc.token)
	}
	if rc.credential != "Basic ZXhwbGljaXQ=" {
		t.Errorf("credential = %q, want the step's", rc.credential)
	}
}

// And an auth-required step WITHOUT a credential must still demand the
// reserved token, so the field cannot become a way to skip authentication.
func TestScenarioStillRequiresTheReservedTokenWithoutACredential(t *testing.T) {
	rc := &recordingCaller{}
	s := Step{
		Endpoint: Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/m", AuthRequired: true},
		Label:    "bearer step",
	}
	err := RunSteps(context.Background(), newScope(), []Step{s}, map[string]Caller{"rest": rc})
	if err == nil {
		t.Fatal("an auth-required step with no captured auth.token and no credential must fail, or the " +
			"credential field becomes a way to run unauthenticated by omission")
	}
}
