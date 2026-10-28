package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fileServer fakes a gateway with an image upload and two download routes
// over the same objects: `/api/files` (public, with a `thumb` variant) and
// `/api/blobs` (behind auth). The upload answers the stored object as both
// routes' URLs, the second in the ABSOLUTE form with a host this server is
// not on. It records the dimensions of every image it was sent.
func fileServer(t *testing.T) (*httptest.Server, *[]image.Point) {
	t.Helper()
	var mu sync.Mutex
	var uploaded []image.Point
	objects := map[string][]byte{}
	refuse := func(w http.ResponseWriter, status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": code}})
	}
	serve := func(w http.ResponseWriter, r *http.Request, key string) {
		mu.Lock()
		b, ok := objects[key]
		mu.Unlock()
		if !ok {
			refuse(w, http.StatusNotFound, "NOT_FOUND")
			return
		}
		switch r.URL.Query().Get("variant") {
		case "":
		case "thumb":
			b, _ = renderImage(ImageFile{Format: "png", Width: 64, Height: 43})
		default:
			refuse(w, http.StatusBadRequest, "INVALID_ARGUMENT")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write(b)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /signin", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-1"})
	})
	mux.HandleFunc("POST /cover", func(w http.ResponseWriter, r *http.Request) {
		f, _, err := r.FormFile("image")
		if err != nil {
			refuse(w, http.StatusBadRequest, "INVALID_ARGUMENT")
			return
		}
		b, _ := io.ReadAll(f)
		cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			refuse(w, http.StatusBadRequest, "INVALID_ARGUMENT")
			return
		}
		mu.Lock()
		uploaded = append(uploaded, image.Pt(cfg.Width, cfg.Height))
		objects["covers/aa/k1"] = b
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image_path":  "/api/files/covers/aa/k1",
			"storage_key": "https://public.example/api/blobs/covers/aa/k1",
		})
	})
	mux.HandleFunc("GET /api/files/", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, strings.TrimPrefix(r.URL.Path, "/api/files/"))
	})
	mux.HandleFunc("GET /api/blobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			refuse(w, http.StatusUnauthorized, "UNAUTHENTICATED")
			return
		}
		serve(w, r, strings.TrimPrefix(r.URL.Path, "/api/blobs/"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &uploaded
}

var fileRoutes = []DownloadRoute{
	{Prefix: "/api/files"},
	{Prefix: "/api/blobs", AuthRequired: true},
}

func num(op string, v float64) map[string]any {
	return map[string]any{"matcher": "num", "op": op, "value": v}
}

func downloadStep(label, url, route, variant string) Step {
	return Step{
		Label:    label,
		Endpoint: Endpoint{Ref: "download " + url, Transport: "rest"},
		Download: &Download{URL: url, Route: route, Variant: variant, Routes: fileRoutes},
	}
}

// The whole round trip a scenario of an image field makes: upload a
// generated image, read it back through the route the response named, ask
// for a variant, ask for one that does not exist, and ask a route behind
// auth without a token.
func TestDownloadStep_RoundTripsAGeneratedImage(t *testing.T) {
	srv, uploaded := fileServer(t)

	upload := Step{
		Label:    "upload",
		Endpoint: ep("a.Covers.Upload", "POST", "/cover", false),
		Files:    []FilePart{{Part: "image", Filename: "cover.png", Image: &ImageFile{Format: "png", Width: 900, Height: 600}}},
		Expect: map[string]any{
			"image_path":  map[string]any{"capture": "cover.url"},
			"storage_key": map[string]any{"capture": "cover.blob"},
		},
	}
	original := downloadStep("original", "${cover.url}", "", "")
	original.Expect = map[string]any{
		"w17_wire.content_type":  "image/png",
		"w17_wire.width":         num("==", 900),
		"w17_wire.height":        num("==", 600),
		"w17_wire.cache_control": "public, max-age=31536000, immutable",
	}
	thumb := downloadStep("thumb", "${cover.url}", "", "thumb")
	thumb.Expect = map[string]any{"w17_wire.width": num("<=", 64), "w17_wire.height": num("<=", 64)}
	undeclared := downloadStep("undeclared variant", "${cover.url}", "", "huge")
	undeclared.ExpectError = &ExpectError{Code: "INVALID_ARGUMENT"}
	signedOut := downloadStep("blobs signed out", "${cover.url}", "/api/blobs", "")
	signedOut.Anonymous = true
	signedOut.ExpectError = &ExpectError{Code: "UNAUTHENTICATED"}
	// The ABSOLUTE URL names another host; the path alone reaches this
	// gateway, and the auth route takes the session token by default.
	blob := downloadStep("blob", "${cover.blob}", "", "")
	blob.Expect = map[string]any{"w17_wire.width": num("==", 900)}

	steps := []Step{signInStep("auth.token"), upload, original, thumb, undeclared, signedOut, blob}
	if err := RunScenario(context.Background(), steps, restCallers(srv)); err != nil {
		t.Fatal(err)
	}
	if len(*uploaded) != 1 || (*uploaded)[0] != image.Pt(900, 600) {
		t.Errorf("the server decoded uploads of %v, want one 900x600 image", *uploaded)
	}
}

// A refusal the step does not expect must fail it: a download route that
// stopped serving a field is exactly what the step exists to notice.
func TestDownloadStep_FailsWhenTheRouteRefuses(t *testing.T) {
	srv, _ := fileServer(t)
	missing := downloadStep("missing", "/api/files/covers/aa/none", "", "")
	missing.Expect = map[string]any{"w17_wire.content_type": "image/png"}
	err := RunScenario(context.Background(), []Step{missing}, restCallers(srv))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the 404 refusal", err)
	}
}

func TestResolveDownload(t *testing.T) {
	scope := newScope()
	scope.Capture("cover.url", "/api/files/covers/aa/k1")
	scope.Capture("cover.abs", "https://cdn.example/api/blobs/covers/aa/k1")
	scope.Capture("cover.proxied", "https://example.com/app/api/files/covers/aa/k1")
	scope.Capture("cover.signed", "https://cdn.example/api/blobs/covers/aa/k1?sig=x")

	for _, tc := range []struct {
		name, url, route, variant string
		wantPath                  string
		wantAuth                  bool
		wantInput                 map[string]any
	}{
		{name: "own route", url: "${cover.url}", wantPath: "/api/files/covers/aa/k1", wantInput: map[string]any{}},
		{name: "variant", url: "${cover.url}", variant: "thumb", wantPath: "/api/files/covers/aa/k1", wantInput: map[string]any{"variant": "thumb"}},
		{name: "absolute keeps only the path", url: "${cover.abs}", wantPath: "/api/blobs/covers/aa/k1", wantAuth: true, wantInput: map[string]any{}},
		{name: "public host with a path", url: "${cover.proxied}", wantPath: "/api/files/covers/aa/k1", wantInput: map[string]any{}},
		{name: "another route", url: "${cover.url}", route: "/api/blobs", wantPath: "/api/blobs/covers/aa/k1", wantAuth: true, wantInput: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, input, err := resolveDownload(Download{URL: tc.url, Route: tc.route, Variant: tc.variant, Routes: fileRoutes}, scope)
			if err != nil {
				t.Fatal(err)
			}
			if got.PathTemplate != tc.wantPath || got.AuthRequired != tc.wantAuth || got.HTTPMethod != "GET" || !got.RawResponse {
				t.Errorf("endpoint = %+v, want GET %s auth=%v raw", got, tc.wantPath, tc.wantAuth)
			}
			if len(input) != len(tc.wantInput) || input["variant"] != tc.wantInput["variant"] {
				t.Errorf("input = %v, want %v", input, tc.wantInput)
			}
		})
	}

	for _, tc := range []struct{ name, url, route, want string }{
		{name: "under no route", url: "/api/other/k1", want: "under no download route"},
		{name: "unknown route", url: "${cover.url}", route: "/api/nope", want: `route "/api/nope" is not a download route`},
		{name: "empty", url: "${cover.none}", want: "expand url"},
		{name: "a query", url: "${cover.signed}", want: "carries a query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := resolveDownload(Download{URL: tc.url, Route: tc.route, Routes: fileRoutes}, scope)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A generated image is the declared format and size, and a format the
// runner cannot encode is an error rather than some other image.
func TestRenderImage(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif"} {
		b, err := renderImage(ImageFile{Format: format, Width: 30, Height: 20})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		cfg, got, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil || got != format || cfg.Width != 30 || cfg.Height != 20 {
			t.Errorf("%s: decoded %s %dx%d (%v), want %s 30x20", format, got, cfg.Width, cfg.Height, err, format)
		}
	}
	if _, err := renderImage(ImageFile{Format: "webp", Width: 1, Height: 1}); err == nil {
		t.Error("webp: no error, want one (the runner does not encode webp)")
	}
	if _, err := renderImage(ImageFile{Format: "png", Width: 0, Height: 1}); err == nil {
		t.Error("0x1: no error, want one")
	}
	if _, err := renderImage(ImageFile{Format: "png", Width: 100000, Height: 100000}); err == nil {
		t.Error("100000x100000: no error, want one rather than gigabytes of pixels")
	}
}
