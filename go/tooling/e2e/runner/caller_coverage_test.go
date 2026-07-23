package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRESTCaller_Defaults(t *testing.T) {
	c := NewRESTCaller("http://host:8080/", nil)
	if c.BaseURL != "http://host:8080" {
		t.Errorf("trailing slash not trimmed: %q", c.BaseURL)
	}
	if c.Client != http.DefaultClient {
		t.Error("nil client should fall back to http.DefaultClient")
	}
}

func TestResolveREST_PathParamMissing(t *testing.T) {
	ep := Endpoint{Ref: "m.S.M", HTTPMethod: "GET", PathTemplate: "/x/{id}", PathParams: []string{"id"}}
	if _, _, _, err := ResolveREST("", ep, map[string]any{}); err == nil {
		t.Fatal("want error when a path param is absent from input")
	}
}

func TestResolveREST_GETQueryExpansion(t *testing.T) {
	ep := Endpoint{
		Ref: "m.S.List", HTTPMethod: "GET", PathTemplate: "/x/{id}",
		PathParams: []string{"id"}, QueryParams: []string{"limit"},
	}
	input := map[string]any{
		"id":     "p1",
		"limit":  10,
		"tags":   []any{"a", "b"},              // repeated query key
		"nested": map[string]any{"skip": true}, // objects are skipped in GET query
		"cursor": "abc",                        // unenumerated scalar → query
	}
	method, target, body, err := ResolveREST("http://h", ep, input)
	if err != nil {
		t.Fatal(err)
	}
	if method != "GET" || body != nil {
		t.Errorf("GET should have no body: method=%s body=%v", method, body)
	}
	if !strings.Contains(target, "/x/p1?") || !strings.Contains(target, "limit=10") ||
		!strings.Contains(target, "tags=a") || !strings.Contains(target, "tags=b") ||
		!strings.Contains(target, "cursor=abc") {
		t.Errorf("query not expanded as expected: %s", target)
	}
}

func TestResolveREST_PostBodyAndBodyField(t *testing.T) {
	// Whole-request body.
	ep := Endpoint{Ref: "m.S.Create", HTTPMethod: "POST", PathTemplate: "/x"}
	_, _, body, err := ResolveREST("", ep, map[string]any{"name": "n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"name":"n"`) {
		t.Errorf("body = %s", body)
	}

	// BodyField selects a sub-message.
	ep2 := Endpoint{Ref: "m.S.Create", HTTPMethod: "POST", PathTemplate: "/x", BodyField: "payload"}
	_, _, body2, err := ResolveREST("", ep2, map[string]any{"payload": map[string]any{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body2), `"k":"v"`) {
		t.Errorf("body-field body = %s", body2)
	}
}

func TestResolveREST_MarshalError(t *testing.T) {
	// A channel can't be JSON-marshalled → body marshal fails on a POST.
	ep := Endpoint{Ref: "m.S.Create", HTTPMethod: "POST", PathTemplate: "/x"}
	if _, _, _, err := ResolveREST("", ep, map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("want marshal error for an unmarshalable body value")
	}
}

func TestRESTCaller_Call_HappyWithAuthAndHeaders(t *testing.T) {
	var gotAuth, gotDevice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDevice = r.Header.Get("X-Device-Id")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	c := NewRESTCaller(srv.URL, srv.Client())
	ep := Endpoint{Ref: "m.S.Create", HTTPMethod: "POST", PathTemplate: "/x"}
	out, err := c.Call(context.Background(), ep, map[string]any{"name": "n"}, "tok",
		map[string]string{"X-Device-Id": "dev1", "Authorization": "Bearer hijack"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out["ok"] != true {
		t.Errorf("decoded out = %v", out)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok (header map must not override it)", gotAuth)
	}
	if gotDevice != "dev1" {
		t.Errorf("device header = %q", gotDevice)
	}
}

func TestRESTCaller_Call_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("x", 1000), http.StatusBadRequest)
	}))
	defer srv.Close()
	c := NewRESTCaller(srv.URL, srv.Client())
	ep := Endpoint{Ref: "m.S.Get", HTTPMethod: "GET", PathTemplate: "/x"}
	if _, err := c.Call(context.Background(), ep, map[string]any{}, "", nil); err == nil {
		t.Fatal("want error for a 4xx status")
	} else if !strings.Contains(err.Error(), "status 400") {
		t.Errorf("error should carry the status: %v", err)
	}
}

func TestRESTCaller_Call_ResolveError(t *testing.T) {
	c := NewRESTCaller("http://h", nil)
	ep := Endpoint{Ref: "m.S.M", HTTPMethod: "GET", PathTemplate: "/x/{id}", PathParams: []string{"id"}}
	if _, err := c.Call(context.Background(), ep, map[string]any{}, "", nil); err == nil {
		t.Fatal("want error when routing can't resolve")
	}
}

func TestRESTCaller_Call_DoError(t *testing.T) {
	// No server listening at this address → Client.Do fails.
	c := NewRESTCaller("http://127.0.0.1:1", &http.Client{})
	ep := Endpoint{Ref: "m.S.Get", HTTPMethod: "GET", PathTemplate: "/x"}
	if _, err := c.Call(context.Background(), ep, map[string]any{}, "", nil); err == nil {
		t.Fatal("want transport error against a dead address")
	}
}

func TestDecodeObject(t *testing.T) {
	// Empty body → empty object.
	out, err := decodeObject([]byte("  "))
	if err != nil || len(out) != 0 {
		t.Errorf("empty decode = %v, %v", out, err)
	}
	// Malformed JSON → error.
	if _, err := decodeObject([]byte("{not json")); err == nil {
		t.Fatal("want decode error for malformed JSON")
	}
	// Valid object.
	out, err = decodeObject([]byte(`{"a":1}`))
	if err != nil || out["a"] != float64(1) {
		t.Errorf("decode = %v, %v", out, err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate([]byte("short"), 10); got != "short" {
		t.Errorf("under-limit = %q", got)
	}
	got := truncate([]byte("abcdef"), 3)
	if got != "abc…" {
		t.Errorf("over-limit = %q, want abc…", got)
	}
}
