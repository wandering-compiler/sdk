package restgw

import (
	"net/http"
	"net/url"
	"strings"
)

// SetNextPageLink (G3-GW-08) writes a `Link: <url>; rel="next"`
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
// URL construction:
//   - Scheme + host come from `r.Host` + the X-Forwarded-Proto
//     header (when behind a reverse proxy that handles TLS;
//     defaults to http otherwise — operators put the gateway
//     behind a TLS-terminating LB and configure it to set
//     X-Forwarded-Proto correctly).
//   - Path is the request path, untouched.
//   - Query: every existing key/value is preserved EXCEPT
//     `cursor` (which gets replaced with the new value).
//     This keeps the user's filters / sort / limit on the
//     same page.
//
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

// nextPageURL rebuilds the request URL with the cursor
// query parameter replaced. Other query parameters are
// preserved.
func nextPageURL(r *http.Request, cursor string) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		// X-Forwarded-Proto can be a comma-separated list across chained
		// proxies ("https, http") — take the first token, and accept only
		// http/https so a multi-value or spoofed header can't inject a
		// malformed scheme into the emitted Link URL.
		first := proto
		if i := strings.IndexByte(proto, ','); i >= 0 {
			first = proto[:i]
		}
		switch s := strings.ToLower(strings.TrimSpace(first)); s {
		case "http", "https":
			scheme = s
		}
	} else if r.TLS != nil {
		scheme = "https"
	}
	q := r.URL.Query()
	q.Set("cursor", cursor)

	// NOTE (restgw-sec-4): the Link URL uses the request Host. The
	// generated service is expected to sit behind an ingress/proxy that
	// sets/validates Host; without one, a client-spoofed Host header is
	// reflected into the advertised next-page URL (Link/cache-poisoning
	// risk for an intermediary that trusts it).
	u := &url.URL{
		Scheme:   scheme,
		Host:     r.Host,
		Path:     r.URL.Path,
		RawQuery: q.Encode(),
	}
	return u.String()
}
