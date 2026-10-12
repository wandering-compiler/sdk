package restgw

import (
	"net/http"
	"strings"
)

// SetNextPageLink writes a `Link: <url>; rel="next"`
// header pointing at the next page of a cursor-paginated
// list response. Authoring shape:
//
//	resp, err := s.backend.ListUsers(ctx, req)
//	if err != nil { ... }
//	if next := computeNextCursor(resp); next != "" {
//	    restgw.SetNextPageLink(w, r, next)
//	}
//	restgw.WriteResponse(w, r, http.StatusOK, resp)
//
// The cursor is the OPAQUE token identifying the next page —
// usually `resp.Users[-1].id` or an encoded
// (last_seen_at, id) tuple. The handler decides what
// produces the cursor; this helper only worries about the
// header shape.
//
// URL construction: the link is ORIGIN-RELATIVE — the request's path with
// its query, every existing key/value preserved EXCEPT `cursor` (which gets
// replaced with the new value), so the user's filters / sort / limit stay on
// the same page. A client resolves it against the URL it requested (RFC
// 8288 §3.2), which is the URL the next page lives at too.
//
// There is no scheme and no host in it on purpose. Both would come from the
// request — `Host` and `X-Forwarded-Proto` — and any caller can write those;
// the gateway cannot tell a terminating proxy's value from a client's. A
// link built from them advertises whatever origin the caller named, which a
// cache that does not key on those headers then serves to everybody else.
// Why a header (not a `next_cursor` proto field): same
// pagination shape composes across every list RPC without
// needing to thread a cursor field through every response
// message. Authors who want both surfaces (header + field)
// can keep the field in the proto AND call this helper.
//
// `Link` header is the IETF RFC 5988 standard and what
// GitHub / GitLab / Stripe use for paginated REST APIs;
// browser tooling (HAL, postman) renders it natively.
func SetNextPageLink(w http.ResponseWriter, r *http.Request, cursor string) {
	if cursor == "" {
		return
	}
	u := nextPageURL(r, cursor)
	w.Header().Set("Link", `<`+u+`>; rel="next"`)
}

// SetPrevPageLink writes the `rel="prev"` companion when the
// API supports backward pagination. Used alongside
// [SetNextPageLink]: callers walking forward set next; an
// API that surfaces both directions (rare; usually only
// page-numbered APIs do) can append both. Header values
// concatenate with a comma per RFC 5988.
func SetPrevPageLink(w http.ResponseWriter, r *http.Request, cursor string) {
	if cursor == "" {
		return
	}
	u := nextPageURL(r, cursor)
	link := `<` + u + `>; rel="prev"`
	if existing := w.Header().Get("Link"); existing != "" {
		link = existing + ", " + link
	}
	w.Header().Set("Link", link)
}

// nextPageURL rebuilds the request URL, origin-relative, with the cursor
// query parameter replaced. Other query parameters are preserved.
func nextPageURL(r *http.Request, cursor string) string {
	q := r.URL.Query()
	q.Set("cursor", cursor)
	// EscapedPath percent-encodes a backslash, which some clients read as
	// a slash. A path that starts with `//` would read as a host, so it is
	// written as `/.//…`, which resolves to the same path on this origin.
	path := r.URL.EscapedPath()
	switch {
	case path == "":
		path = "/"
	case strings.HasPrefix(path, "//"):
		path = "/." + path
	}
	return path + "?" + q.Encode()
}
