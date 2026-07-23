package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// REV-149 — MetadataRenameMiddleware + MetadataDefaultStampMiddleware.

func TestMetadataRenameMiddleware_AppendsRenamedKey(t *testing.T) {
	rules := []restgw.HeaderRenameRule{
		{HTTP: "Accept-Language", Metadata: "w17-language"},
	}
	var captured metadata.MD
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = metadata.FromOutgoingContext(r.Context())
	})
	srv := httptest.NewServer(restgw.MetadataRenameMiddleware(rules, next))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Language", "cs-CZ")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	vals := captured.Get("w17-language")
	if len(vals) != 1 || vals[0] != "cs-CZ" {
		t.Errorf("w17-language metadata = %v, want [cs-CZ]", vals)
	}
}

func TestMetadataRenameMiddleware_SkipsAbsentHeader(t *testing.T) {
	rules := []restgw.HeaderRenameRule{
		{HTTP: "Accept-Language", Metadata: "w17-language"},
	}
	var captured metadata.MD
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = metadata.FromOutgoingContext(r.Context())
	})
	srv := httptest.NewServer(restgw.MetadataRenameMiddleware(rules, next))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	if vals := captured.Get("w17-language"); len(vals) != 0 {
		t.Errorf("w17-language metadata = %v, want empty (header absent)", vals)
	}
}

func TestMetadataRenameMiddleware_PassThroughOnEmptyRules(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	got := restgw.MetadataRenameMiddleware(nil, next)
	if any, ok := got.(http.HandlerFunc); !ok || any == nil {
		t.Fatalf("MetadataRenameMiddleware(nil) = %T, want pass-through http.HandlerFunc", got)
	}
	rr := httptest.NewRecorder()
	got.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Error("next handler not invoked on empty-rules pass-through")
	}
}

func TestMetadataDefaultStampMiddleware_FillsAbsentKey(t *testing.T) {
	stamps := []restgw.DefaultMetadataStamp{
		{Key: "w17-language", Value: "cs"},
	}
	var captured metadata.MD
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = metadata.FromOutgoingContext(r.Context())
	})
	srv := httptest.NewServer(restgw.MetadataDefaultStampMiddleware(stamps, next))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	vals := captured.Get("w17-language")
	if len(vals) != 1 || vals[0] != "cs" {
		t.Errorf("w17-language metadata = %v, want [cs]", vals)
	}
}

func TestMetadataDefaultStampMiddleware_SkipsAlreadySetKey(t *testing.T) {
	// Upstream rename middleware sets w17-language=en first,
	// then default stamp should NOT overwrite to "cs".
	stamps := []restgw.DefaultMetadataStamp{
		{Key: "w17-language", Value: "cs"},
	}
	var captured metadata.MD
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = metadata.FromOutgoingContext(r.Context())
	})
	chain := restgw.MetadataRenameMiddleware(
		[]restgw.HeaderRenameRule{{HTTP: "Accept-Language", Metadata: "w17-language"}},
		restgw.MetadataDefaultStampMiddleware(stamps, next),
	)
	srv := httptest.NewServer(chain)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Language", "en")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	vals := captured.Get("w17-language")
	if len(vals) != 1 || vals[0] != "en" {
		t.Errorf("w17-language metadata = %v, want [en] (rename wins; default skipped)", vals)
	}
}

func TestMetadataDefaultStampMiddleware_PassThroughOnEmptyStamps(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	got := restgw.MetadataDefaultStampMiddleware(nil, next)
	rr := httptest.NewRecorder()
	got.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Error("next handler not invoked on empty-stamps pass-through")
	}
}
