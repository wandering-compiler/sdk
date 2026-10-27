package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func rawResult(t *testing.T, method string, hdr map[string]string, resp restgw.RawResponse) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/public/invoices/tok/pdf", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	restgw.WriteRawResponse(rec, req, resp)
	return rec
}

// A link to the endpoint is the file: the body verbatim, its type, a file
// name the browser saves it under, and a validator a revisit can use.
func TestWriteRawResponse_TheBodyIsTheFile(t *testing.T) {
	rec := rawResult(t, http.MethodGet, nil, restgw.RawResponse{
		Body:        []byte("%PDF-1.7"),
		ContentType: "application/pdf",
		Filename:    "faktura-2026-001.pdf",
	})
	if rec.Code != http.StatusOK || rec.Body.String() != "%PDF-1.7" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	for k, want := range map[string]string{
		"Content-Type":           "application/pdf",
		"Content-Length":         "8",
		"Content-Disposition":    `attachment; filename=faktura-2026-001.pdf`,
		"Cache-Control":          "private, no-cache",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if et := h.Get("ETag"); len(et) != 66 || et[0] != '"' {
		t.Errorf("ETag = %q, want a quoted sha256", et)
	}
}

// The ETag saves the transfer: a revalidation naming it gets 304, no body.
func TestWriteRawResponse_IfNoneMatchIs304(t *testing.T) {
	resp := restgw.RawResponse{Body: []byte("same bytes"), ContentType: "text/csv"}
	etag := rawResult(t, http.MethodGet, nil, resp).Header().Get("ETag")
	rec := rawResult(t, http.MethodGet, map[string]string{"If-None-Match": etag}, resp)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("revalidation = %d with %d bytes, want 304 and none", rec.Code, rec.Body.Len())
	}
	changed := rawResult(t, http.MethodGet, map[string]string{"If-None-Match": etag}, restgw.RawResponse{Body: []byte("new bytes")})
	if changed.Code != http.StatusOK {
		t.Errorf("a changed body answered %d to the old ETag", changed.Code)
	}
}

func TestWriteRawResponse_RangeAndHead(t *testing.T) {
	resp := restgw.RawResponse{Body: []byte("0123456789")}
	rec := rawResult(t, http.MethodGet, map[string]string{"Range": "bytes=2-4"}, resp)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" {
		t.Errorf("range = %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("untyped body Content-Type = %q", ct)
	}
	head := rawResult(t, http.MethodHead, nil, resp)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "10" {
		t.Errorf("HEAD = %d, %d bytes, length %q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}
}

// The file name is handler DATA: a path or a control character in it must
// not reach the header, and a non-ASCII name travels in the form browsers
// decode.
func TestWriteRawResponse_FilenameIsSanitised(t *testing.T) {
	for _, c := range []struct {
		name   string
		inline bool
		want   string
	}{
		{"../../etc/passwd", false, "attachment; filename=passwd"},
		{"a\r\nSet-Cookie: x.pdf", false, `attachment; filename="aSet-Cookie: x.pdf"`},
		{"účtenka.pdf", true, "inline; filename*=utf-8''%C3%BA%C4%8Dtenka.pdf"},
		{"", true, "inline"},
		{"", false, ""},
		{"..", false, ""},
	} {
		rec := rawResult(t, http.MethodGet, nil, restgw.RawResponse{Body: []byte("x"), Filename: c.name, Inline: c.inline})
		if got := rec.Header().Get("Content-Disposition"); got != c.want {
			t.Errorf("filename %q inline=%v: Content-Disposition = %q, want %q", c.name, c.inline, got, c.want)
		}
	}
}

func TestWriteRawResponse_CacheControlOverride(t *testing.T) {
	rec := rawResult(t, http.MethodGet, nil, restgw.RawResponse{Body: []byte("x"), CacheControl: "no-store"})
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

// The type can come from a response field — handler data — so a value that
// is not a media type falls back instead of reaching the header.
func TestWriteRawResponse_InvalidContentTypeFallsBack(t *testing.T) {
	for _, ct := range []string{"pdf", "text/plain; =broken"} {
		rec := rawResult(t, http.MethodGet, nil, restgw.RawResponse{Body: []byte("x"), ContentType: ct})
		if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("ContentType %q sent as %q, want application/octet-stream", ct, got)
		}
	}
}

// A non-GET call already ran its handler: a conditional or a Range must not
// turn its result into a 412 or a fragment.
func TestWriteRawResponse_NonGetAlwaysSendsTheWholeBody(t *testing.T) {
	resp := restgw.RawResponse{Body: []byte("0123456789"), ContentType: "text/plain"}
	etag := rawResult(t, http.MethodGet, nil, resp).Header().Get("ETag")
	rec := rawResult(t, http.MethodPost, map[string]string{"If-None-Match": etag, "Range": "bytes=2-4"}, resp)
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" || rec.Header().Get("Content-Length") != "10" {
		t.Fatalf("POST = %d %q (length %q), want 200 with the whole body", rec.Code, rec.Body.String(), rec.Header().Get("Content-Length"))
	}
}

// A scriptable type can come from handler data: it is sandboxed, so the body
// never runs as a page of the API's origin. A PDF is not.
func TestWriteRawResponse_ScriptableTypesAreSandboxed(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/html; charset=utf-8": true,
		"image/svg+xml":            true,
		"application/xhtml+xml":    true,
		"application/pdf":          false,
		"text/plain":               false,
	} {
		rec := rawResult(t, http.MethodGet, nil, restgw.RawResponse{Body: []byte("x"), ContentType: ct, Inline: true})
		if got := rec.Header().Get("Content-Security-Policy") != ""; got != want {
			t.Errorf("%s: sandboxed = %v, want %v", ct, got, want)
		}
	}
}

// A bidi override or C1 control in a handler's file name would let the save
// dialog show a different extension than the one saved.
func TestWriteRawResponse_FilenameDropsFormatCharacters(t *testing.T) {
	rec := rawResult(t, http.MethodGet, nil, restgw.RawResponse{Body: []byte("x"), Filename: "invoice\u202efdp.exe\u0085"})
	if got, want := rec.Header().Get("Content-Disposition"), "attachment; filename=invoicefdp.exe"; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
}
