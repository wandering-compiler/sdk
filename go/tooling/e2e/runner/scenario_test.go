package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// projectServer fakes a deployed REST gateway, recording created names
// so the test can assert sequence threading + capture flow.
func projectServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var created []string
	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/signin", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "tok-123"})
	})
	mux.HandleFunc("POST /projects", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		created = append(created, body.Name)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id": "proj-" + body.Name})
	})
	mux.HandleFunc("GET /projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "name": "demo"})
	})
	mux.HandleFunc("GET /projects", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"projects": []any{map[string]any{"id": 1}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &created
}

func ep(ref, method, path string, auth bool, pathParams ...string) Endpoint {
	return Endpoint{Ref: ref, Transport: "rest", AuthRequired: auth, HTTPMethod: method, PathTemplate: path, PathParams: pathParams}
}

func TestRunScenario_Success(t *testing.T) {
	srv, created := projectServer(t)
	callers := map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())}

	steps := []Step{
		{ // login → capture token (used by later auth steps)
			Endpoint: ep("forge.Auth.SignIn", "POST", "/auth/signin", false),
			Input:    map[string]any{"email": "a@b.c", "password": "x"},
			Expect:   map[string]any{"token": map[string]any{"capture": "auth.token"}},
		},
		{ // create → capture id
			Endpoint: ep("forge.P.CreateProject", "POST", "/projects", false),
			Input:    map[string]any{"name": "demo"},
			Expect:   map[string]any{"id": map[string]any{"capture": "project.id"}},
		},
		{ // auth-required get using the captured id (capture flow across steps)
			Endpoint: ep("forge.P.GetProject", "GET", "/projects/{id}", true, "id"),
			Input:    map[string]any{"id": "${project.id}"},
			Expect:   map[string]any{"id": "${project.id}", "name": map[string]any{"matcher": "not_empty"}},
		},
		{ // auth-required list, count matcher
			Endpoint: ep("forge.P.ListProjects", "GET", "/projects", true),
			Expect:   map[string]any{"projects": map[string]any{"matcher": "count", "op": ">=", "value": 1}},
		},
		{ // bulk create, repeat with ${seq}
			Endpoint: ep("forge.P.CreateProject", "POST", "/projects", false),
			Input:    map[string]any{"name": "proj${seq}"},
			Expect:   map[string]any{"id": map[string]any{"matcher": "not_empty"}},
			Repeat:   3,
		},
	}

	if err := RunScenario(context.Background(), steps, callers); err != nil {
		t.Fatalf("scenario failed: %v", err)
	}

	// demo (step 2) + proj1/proj2/proj3 (step 5 ×3 with global seq)
	want := map[string]bool{"demo": true, "proj1": true, "proj2": true, "proj3": true}
	for _, n := range *created {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Errorf("missing created names %v (got %v)", want, *created)
	}
}

func TestRunScenario_FailAborts(t *testing.T) {
	srv, _ := projectServer(t)
	callers := map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())}
	// second step asserts a wrong value → scenario fails fast
	steps := []Step{
		{Endpoint: ep("forge.Auth.SignIn", "POST", "/auth/signin", false),
			Expect: map[string]any{"token": map[string]any{"capture": "auth.token"}}},
		{Endpoint: ep("forge.P.CreateProject", "POST", "/projects", false),
			Input:  map[string]any{"name": "demo"},
			Expect: map[string]any{"id": "WRONG"}},
	}
	if err := RunScenario(context.Background(), steps, callers); err == nil {
		t.Error("expected scenario to fail on the mismatched expect")
	}
}

func TestRunScenario_MissingAuthToken(t *testing.T) {
	srv, _ := projectServer(t)
	callers := map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())}
	// auth-required step with no prior token capture → fail
	steps := []Step{
		{Endpoint: ep("forge.P.GetProject", "GET", "/projects/{id}", true, "id"),
			Input: map[string]any{"id": "x"}},
	}
	if err := RunScenario(context.Background(), steps, callers); err == nil {
		t.Error("expected failure: auth-required step with no captured token")
	}
}
