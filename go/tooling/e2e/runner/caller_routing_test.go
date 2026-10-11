package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Input a bodyless verb cannot carry is an error, not silently dropped:
// the step would otherwise assert against an unfiltered request.
func TestResolveREST_NestedInputOnBodylessVerbIsRefused(t *testing.T) {
	for _, method := range []string{"GET", "DELETE"} {
		for name, v := range map[string]any{
			"object":          map[string]any{"status": "open"},
			"list of objects": []any{map[string]any{"status": "open"}},
		} {
			ep := Endpoint{Ref: "m.S.List", HTTPMethod: method, PathTemplate: "/l"}
			_, target, _, err := ResolveREST("", ep, map[string]any{"filter": v, "limit": 5})
			if err == nil {
				t.Errorf("%s %s: nested input dropped, sent %s", method, name, target)
				continue
			}
			if !strings.Contains(err.Error(), `"filter"`) {
				t.Errorf("%s %s: error must name the field: %v", method, name, err)
			}
		}
	}
	// A declared query param is held to the same rule.
	ep := Endpoint{Ref: "m.S.Create", HTTPMethod: "POST", PathTemplate: "/c", QueryParams: []string{"opts"}}
	if _, _, _, err := ResolveREST("", ep, map[string]any{"opts": map[string]any{"a": 1}}); err == nil {
		t.Error("an object bound to a declared query param must be refused")
	}
	// So is a path param.
	ep = Endpoint{Ref: "m.S.Get", HTTPMethod: "GET", PathTemplate: "/x/{id}", PathParams: []string{"id"}}
	if _, _, _, err := ResolveREST("", ep, map[string]any{"id": map[string]any{"v": 1}}); err == nil {
		t.Error("an object bound to a path param must be refused")
	}
}

// A captured JSON number arrives as float64; routed into a path or query it
// must read as the integer it is, not 1.234567e+06.
func TestResolveREST_NumbersRenderWithoutExponent(t *testing.T) {
	ep := Endpoint{Ref: "m.S.Get", HTTPMethod: "GET", PathTemplate: "/x/{id}", PathParams: []string{"id"}, QueryParams: []string{"tags"}}
	_, target, _, err := ResolveREST("", ep, map[string]any{"id": 1234567.0, "tags": []any{"a", 7654321.0}, "skip": nil})
	if err != nil {
		t.Fatal(err)
	}
	if target != "/x/1234567?tags=a&tags=7654321" {
		t.Errorf("target = %s", target)
	}
}

// Multipart sends the BODY's fields only: path and query bindings are not
// repeated as form fields, and body_field narrows the form to that
// sub-message.
func TestCall_MultipartSendsOnlyBodyFields(t *testing.T) {
	var gotPath, gotQuery string
	var gotForm map[string][]string
	var gotFiles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotForm = r.MultipartForm.Value
		gotFiles = nil
		for k := range r.MultipartForm.File {
			gotFiles = append(gotFiles, k)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c := NewRESTCaller(srv.URL, srv.Client())
	files := []FilePart{{Part: "file", Filename: "a.txt", Content: "hi"}}

	ep := Endpoint{Ref: "m.S.Upload", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/p/{id}",
		PathParams: []string{"id"}, QueryParams: []string{"mode"}}
	if _, err := c.Call(context.Background(), ep, map[string]any{"id": 42, "mode": "fast", "title": "t"}, "", nil, "", files); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/p/42" || gotQuery != "mode=fast" {
		t.Errorf("routing: path=%s query=%s", gotPath, gotQuery)
	}
	if len(gotForm) != 1 || gotForm["title"][0] != "t" {
		t.Errorf("form fields = %v, want only title (path/query must not repeat)", gotForm)
	}
	if len(gotFiles) != 1 || gotFiles[0] != "file" {
		t.Errorf("file parts = %v", gotFiles)
	}

	ep.BodyField = "meta"
	in := map[string]any{"id": 42, "mode": "fast", "meta": map[string]any{"title": "t", "lang": "en"}}
	if _, err := c.Call(context.Background(), ep, in, "", nil, "", files); err != nil {
		t.Fatal(err)
	}
	if len(gotForm) != 2 || gotForm["title"][0] != "t" || gotForm["lang"][0] != "en" {
		t.Errorf("body_field form fields = %v, want the sub-message's fields", gotForm)
	}
}
