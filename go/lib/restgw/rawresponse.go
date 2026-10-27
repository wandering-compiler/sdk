package restgw

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// RawResponse is how an endpoint declared `raw_response` answers: the
// handler's bytes as the body, with the headers its declaration names.
// The generated handler fills it from the response message.
type RawResponse struct {
	// Body is sent verbatim.
	Body []byte
	// ContentType is the body's media type. Empty, or not a media type →
	// application/octet-stream.
	ContentType string
	// Filename, when non-empty, is sent as the Content-Disposition file
	// name.
	Filename string
	// Inline asks the browser to show the body rather than save it.
	Inline bool
	// CacheControl is the Cache-Control value. Empty → "private, no-cache".
	CacheControl string
}

// WriteRawResponse answers r with resp: 200 with the body, a strong ETag
// (the body's sha256) and the headers resp names; 304 when the request's
// If-None-Match names that ETag; 206 for a satisfiable Range; no body for
// HEAD. http.ServeContent does the conditional and range work — for GET and
// HEAD only. Any other method already ran its handler (a side effect the
// caller cannot repeat for free), so it always gets the whole body with a
// 200: ServeContent would answer a matching If-None-Match with a 412 and a
// Range with a 206, throwing away the result of the call that ran.
//
// A scriptable type (HTML, SVG, XML) is sent with a sandboxing
// Content-Security-Policy: the type can come from handler data, and the
// body would otherwise run as a page of the API's own origin.
func WriteRawResponse(w http.ResponseWriter, r *http.Request, resp RawResponse) {
	h := w.Header()
	// The type can come from a response FIELD (content_type_field) — data
	// the parser never saw — so a value that is not a media type falls back
	// rather than reaching the header as sent.
	ct := resp.ContentType
	if _, _, err := mime.ParseMediaType(ct); ct == "" || err != nil || !strings.Contains(ct, "/") {
		ct = "application/octet-stream"
	}
	// Set before ServeContent, which sniffs the body only when no type
	// is set; nosniff keeps the browser to the type it is given.
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	if scriptableMediaType(ct) {
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	}
	sum := sha256.Sum256(resp.Body)
	h.Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	cc := resp.CacheControl
	if cc == "" {
		cc = "private, no-cache"
	}
	h.Set("Cache-Control", cc)
	if d := contentDisposition(resp.Filename, resp.Inline); d != "" {
		h.Set("Content-Disposition", d)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Content-Length", strconv.Itoa(len(resp.Body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(resp.Body)
		return
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(resp.Body))
}

// scriptableMediaType reports whether a browser runs a body of type ct as a
// document that can execute script: HTML, XHTML, SVG and the XML family.
func scriptableMediaType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return true
	}
	switch mt {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml", "text/xsl":
		return true
	}
	return strings.HasSuffix(mt, "+xml")
}

// contentDisposition renders the header for a file name the HANDLER
// returned — data, so it is reduced to its last path segment and stripped
// of control characters before it reaches a header. mime.FormatMediaType
// quotes it, and encodes a non-ASCII name in the RFC 2231 `filename*` form
// browsers read. Empty name: inline alone, or nothing (the browser
// decides).
func contentDisposition(filename string, inline bool) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	name := sanitizeFilename(filename)
	if name == "" {
		if inline {
			return kind
		}
		return ""
	}
	if d := mime.FormatMediaType(kind, map[string]string{"filename": name}); d != "" {
		return d
	}
	return kind
}

func sanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	// Control characters (C0, DEL, C1) and format characters: a bidi
	// override (U+202E) in "invoice\u202efdp.exe" shows the browser's save
	// dialog "invoiceexe.pdf".
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "." || name == ".." {
		return ""
	}
	return name
}
