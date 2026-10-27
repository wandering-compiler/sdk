package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A raw_response endpoint answers with the file itself. The runner must not
// decode it — a text body that happens to be JSON is still a file — and
// reports it through WireKey: the body, its type, its disposition, its ETag.
func TestRESTCaller_RawResponseIsReportedThroughTheWire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "inline; filename=note-1.txt")
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte(`{"looks":"like json"}`))
	}))
	defer srv.Close()
	c := NewRESTCaller(srv.URL, nil)
	ep := Endpoint{Ref: "m.S.Text", HTTPMethod: "GET", PathTemplate: "/text", RawResponse: true}
	out, err := c.Call(context.Background(), ep, map[string]any{}, "", nil, "", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if _, decoded := out["looks"]; decoded {
		t.Fatal("a raw response was decoded into fields")
	}
	wire, _ := out[WireKey].(map[string]any)
	for k, want := range map[string]any{
		"content_type":        "text/plain",
		"body":                `{"looks":"like json"}`,
		"content_disposition": "inline; filename=note-1.txt",
		"etag":                `"abc"`,
		"size":                float64(21),
	} {
		if wire[k] != want {
			t.Errorf("%s.%s = %v, want %v", WireKey, k, wire[k], want)
		}
	}
}
