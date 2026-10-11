package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// authServer fakes a gateway with a sign-in and one auth-required read.
// signinToken is what sign-in answers with (nil = a sign-in that returns
// no token). refuseAll makes the read refuse EVERY caller, signed in or
// not — the server against which a refusal case must not pass for the
// wrong reason. It records each Authorization header the read saw.
func authServer(t *testing.T, signinToken any, refuseAll bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /signin", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"user_id": "u1"}
		if signinToken != nil {
			body["token"] = signinToken
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if refuseAll || !strings.HasPrefix(auth, "Bearer tok-") {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "UNAUTHENTICATED", "message": "sign in"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user_id": "u1", "auth": auth})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func signInStep(captureAs string) Step {
	return Step{
		Label:    "sign in",
		Endpoint: ep("a.Auth.SignIn", "POST", "/signin", false),
		Input:    map[string]any{"email": "a@b.c"},
		Expect:   map[string]any{"token": map[string]any{"capture": captureAs}},
	}
}

func meStep() Step {
	return Step{Label: "me", Endpoint: ep("a.Auth.Me", "GET", "/me", true)}
}

func restCallers(srv *httptest.Server) map[string]Caller {
	return map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())}
}

// An anonymous step reaches an auth-required endpoint with no
// Authorization header at all, even after the scenario signed in — so a
// signed-out refusal can be asserted.
func TestAnonymousStep_SendsNoCredential(t *testing.T) {
	srv, seen := authServer(t, "tok-1", false)
	signedOut := meStep()
	signedOut.Anonymous = true
	signedOut.ExpectError = &ExpectError{Code: "UNAUTHENTICATED"}
	signedIn := meStep()
	signedIn.Expect = map[string]any{"auth": "Bearer tok-1"}

	err := RunScenario(context.Background(), []Step{signInStep("auth.token"), signedOut, signedIn}, restCallers(srv))
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 2 || (*seen)[0] != "" || (*seen)[1] != "Bearer tok-1" {
		t.Errorf("Authorization headers seen = %q, want [\"\" \"Bearer tok-1\"]", *seen)
	}
}

// A step that expects UNAUTHENTICATED while implicitly carrying the session
// token cannot pass: against a server that refuses every signed-in call
// it would, and that refusal says nothing about signed-out callers.
func TestImplicitSessionToken_CannotAssertUnauthenticated(t *testing.T) {
	srv, seen := authServer(t, "tok-1", true)
	step := meStep()
	step.ExpectError = &ExpectError{Code: "UNAUTHENTICATED"}

	err := RunScenario(context.Background(), []Step{signInStep("auth.token"), step}, restCallers(srv))
	if err == nil {
		t.Fatal("an UNAUTHENTICATED expectation on a signed-in call passed")
	}
	if !strings.Contains(err.Error(), "anonymous") || !strings.Contains(err.Error(), "token_from") {
		t.Errorf("the failure must name both ways to say what is meant: %v", err)
	}
	if len(*seen) != 0 {
		t.Errorf("the ambiguous step must fail before sending; saw %d request(s)", len(*seen))
	}

	// Naming the token on purpose (the "this session is refused" case)
	// is allowed and runs.
	step.TokenFrom = "auth.token"
	if err := RunScenario(context.Background(), []Step{signInStep("auth.token"), step}, restCallers(srv)); err != nil {
		t.Errorf("an explicitly named token may assert UNAUTHENTICATED: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0] != "Bearer tok-1" {
		t.Errorf("Authorization seen = %q, want the named token", *seen)
	}
}

// TokenFrom presents a different captured token than auth.token.
func TestTokenFrom_UsesTheNamedCapture(t *testing.T) {
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /signin", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email string `json:"email"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-" + in.Email})
	})
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"user_id": "u1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	first := signInStep("auth.token")
	first.Input = map[string]any{"email": "alice"}
	second := signInStep("other.token")
	second.Input = map[string]any{"email": "bob"}
	step := meStep()
	step.TokenFrom = "other.token"
	step.Expect = map[string]any{"user_id": "u1"}
	if err := RunScenario(context.Background(), []Step{first, second, step}, restCallers(srv)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "Bearer tok-bob" {
		t.Errorf("Authorization seen = %q, want the token_from capture", seen)
	}
}

func TestTokenFrom_UnboundOrEmptyFailsBeforeSending(t *testing.T) {
	srv, seen := authServer(t, "", false)
	step := meStep()
	step.TokenFrom = "nobody.token"
	step.Expect = map[string]any{"user_id": "u1"}
	if err := RunScenario(context.Background(), []Step{step}, restCallers(srv)); err == nil || !strings.Contains(err.Error(), "nobody.token") {
		t.Errorf("unbound token_from must fail naming it: %v", err)
	}
	// Sign-in answering token "" binds an empty token: the next
	// auth-required call must not go out as `Bearer `.
	plain := meStep()
	plain.Expect = map[string]any{"user_id": "u1"}
	err := RunScenario(context.Background(), []Step{signInStep("auth.token"), plain}, restCallers(srv))
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty captured token must fail the step: %v", err)
	}
	if len(*seen) != 0 {
		t.Errorf("no request may be sent with an empty token; saw %q", *seen)
	}
}

// A sign-in that returns no token fails at the capture, instead of every
// later step riding `Bearer <nil>` and every expected refusal passing.
func TestSignInWithoutToken_FailsAtTheCapture(t *testing.T) {
	srv, seen := authServer(t, nil, false)
	step := meStep()
	step.TokenFrom = "auth.token"
	step.ExpectError = &ExpectError{Code: "UNAUTHENTICATED"}
	err := RunScenario(context.Background(), []Step{signInStep("auth.token"), step}, restCallers(srv))
	if err == nil || !strings.Contains(err.Error(), "step 1") || !strings.Contains(err.Error(), "field absent") {
		t.Fatalf("want the sign-in step to fail on the missing token, got %v", err)
	}
	if len(*seen) != 0 {
		t.Errorf("no later step may run; saw %q", *seen)
	}
}

func TestStepCredentialFlags_Contradictions(t *testing.T) {
	srv, _ := authServer(t, "tok-1", false)
	for name, mut := range map[string]func(*Step){
		"anonymous+credential":  func(s *Step) { s.Anonymous = true; s.Credential = "Basic x" },
		"anonymous+token_from":  func(s *Step) { s.Anonymous = true; s.TokenFrom = "auth.token" },
		"credential+token_from": func(s *Step) { s.Credential = "Basic x"; s.TokenFrom = "auth.token" },
	} {
		step := meStep()
		step.Expect = map[string]any{"user_id": "u1"}
		mut(&step)
		if err := RunScenario(context.Background(), []Step{signInStep("auth.token"), step}, restCallers(srv)); err == nil {
			t.Errorf("%s: contradictory credential flags were accepted", name)
		}
	}
}
