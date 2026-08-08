package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

// capabilityLinkServer fakes a gateway hosting one REV-162 capability-link
// endpoint. It records whether the caller sent an Authorization header,
// which is the whole point of these tests: a capability link is opened by
// someone with no session, and a case that quietly carried a bearer token
// would pass while exercising a request no real recipient makes.
func capabilityLinkServer(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var sawAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /public/notes/{token}", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"title": "note for " + r.PathValue("token")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sawAuth
}

func credentialStep(target string) (Step, Caller) {
	endpoint := Endpoint{
		Ref:             "tasks.TaskQuery.GetNoteByPublicToken",
		Transport:       "rest",
		AuthRequired:    true,
		CredentialInURL: true,
		HTTPMethod:      "GET",
		PathTemplate:    "/public/notes/{token}",
		PathParams:      []string{"token"},
	}
	step := Step{
		Label:    "public note by capability token",
		Endpoint: endpoint,
		Input:    map[string]any{"token": "tok-capability"},
		Expect:   map[string]any{"title": map[string]any{"matcher": "not_empty"}},
	}
	return step, &RESTCaller{BaseURL: target, Client: http.DefaultClient}
}

// A capability-link step must run in a scenario that never signed anyone
// in. Before REV-162 the runner demanded `auth.token` for any endpoint
// with AuthRequired, so this scenario could not be expressed at all — the
// step failed before reaching the gateway.
func TestRunStep_CapabilityLink_NeedsNoUpstreamAuth(t *testing.T) {
	srv, sawAuth := capabilityLinkServer(t)
	step, caller := credentialStep(srv.URL)

	scope := runtime.NewRun().NewScope()
	err := runStep(context.Background(), scope, step, map[string]Caller{"rest": caller}, runCfg{})
	if err != nil {
		t.Fatalf("runStep: %v", err)
	}
	if *sawAuth != "" {
		t.Errorf("gateway saw Authorization %q; a capability link's recipient has no session, so the runner must send none", *sawAuth)
	}
}

// Even when the scenario HAS a token in scope — the common case, since a
// fixture creating the note signs in first — the capability step must not
// borrow it. Otherwise the case would keep passing if the URL credential
// stopped working entirely, because the bearer header would carry it.
func TestRunStep_CapabilityLink_DoesNotBorrowScenarioToken(t *testing.T) {
	srv, sawAuth := capabilityLinkServer(t)
	step, caller := credentialStep(srv.URL)

	scope := runtime.NewRun().NewScope()
	scope.Capture("auth.token", "tok-from-an-earlier-fixture")

	if err := runStep(context.Background(), scope, step, map[string]Caller{"rest": caller}, runCfg{}); err != nil {
		t.Fatalf("runStep: %v", err)
	}
	if *sawAuth != "" {
		t.Errorf("gateway saw Authorization %q; the step must ignore the scenario's captured token", *sawAuth)
	}
}

// The sibling behaviour must not regress: an ordinary auth-required
// endpoint still demands a token upstream. Without this, "does not
// require auth.token" could be satisfied by dropping the requirement
// everywhere — a green suite that authenticates nothing.
func TestRunStep_OrdinaryAuthEndpoint_StillRequiresToken(t *testing.T) {
	srv, _ := capabilityLinkServer(t)
	step, caller := credentialStep(srv.URL)
	step.Endpoint.CredentialInURL = false

	err := runStep(context.Background(), runtime.NewRun().NewScope(), step, map[string]Caller{"rest": caller}, runCfg{})
	if err == nil {
		t.Fatal("runStep: want an error for an auth-required endpoint with no captured token; got nil")
	}
	if !strings.Contains(err.Error(), "auth.token") {
		t.Errorf("error = %q, want it to name the missing capture", err.Error())
	}
}

// And an ordinary auth-required endpoint must still SEND the token.
func TestRunStep_OrdinaryAuthEndpoint_SendsBearer(t *testing.T) {
	srv, sawAuth := capabilityLinkServer(t)
	step, caller := credentialStep(srv.URL)
	step.Endpoint.CredentialInURL = false

	scope := runtime.NewRun().NewScope()
	scope.Capture("auth.token", "tok-123")
	if err := runStep(context.Background(), scope, step, map[string]Caller{"rest": caller}, runCfg{}); err != nil {
		t.Fatalf("runStep: %v", err)
	}
	if *sawAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", *sawAuth, "Bearer tok-123")
	}
}
