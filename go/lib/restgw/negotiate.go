package restgw

import (
	"net/http"
	"strconv"
	"strings"
)

// Content negotiation (C9). The REST gateway speaks JSON by
// default and binary protobuf on demand. The format is chosen
// per request from the standard HTTP headers — JSON clients
// (curl, browsers, existing FE) never send the protobuf media
// type, so they keep the byte-for-byte JSON behaviour; the
// generated PB client opts in via Accept / Content-Type.
//
// Errors are NOT negotiated: every error envelope stays JSON
// regardless of Accept (see WriteError*). The client tells
// success from error by the *response* Content-Type, so a 4xx/5xx
// JSON body under a protobuf Accept is unambiguous.
//
// SSE is NOT negotiated either: it's a text protocol (`data:
// <json>\n\n`) and base64-wrapping protobuf would be larger than
// the JSON it replaces. SSE events stay JSON; binary rides only
// unary responses and WebSocket frames.

// MIME types for the protobuf wire. `application/protobuf` is the
// canonical one emitted on responses; `application/x-protobuf` is
// accepted as an alias on input for clients/proxies that use the
// older spelling.
const (
	MIMEProtobuf      = "application/protobuf"
	MIMEProtobufXAlt  = "application/x-protobuf"
	MIMEJSON          = "application/json"
	wsSubprotocolPB   = "w17.pb"
	wsSubprotocolJSON = "w17.json"
)

// WireFormat selects the on-the-wire encoding for one request /
// response. The zero value is WireJSON so any un-negotiated path
// defaults to the historical JSON behaviour.
type WireFormat int

const (
	// WireJSON — protojson encoding (the default + only format
	// pre-C9). Content-Type `application/json`.
	WireJSON WireFormat = iota
	// WireProto — binary protobuf encoding. Content-Type
	// `application/protobuf`.
	WireProto
)

// RequestFormat picks the decode format for an inbound request
// body from its Content-Type header. Anything that isn't the
// protobuf media type (including an empty header, `application/
// json`, or form types) decodes as JSON — so GET requests and
// legacy JSON callers are unaffected.
func RequestFormat(r *http.Request) WireFormat {
	if r == nil {
		return WireJSON
	}
	if isProtobufMedia(mediaType(r.Header.Get("Content-Type"))) {
		return WireProto
	}
	return WireJSON
}

// ResponseFormat picks the encode format for the response from
// the request's Accept header, honouring the client's quality
// weights (`;q=`). Among the media types the gateway recognises —
// a protobuf media type → WireProto; `application/json`,
// `application/*`, or `*/*` → WireJSON — the highest-weighted one
// wins. Ties on q break toward the more specific media type
// (exact > `type/*` > `*/*`), then toward the earlier list
// position (preserving the pre-q "first listed wins" behaviour).
// A `;q=0` weight marks a type explicitly unacceptable and drops
// it from the running. An absent / unrecognised Accept defaults
// to JSON (curl-friendly, backward compatible).
func ResponseFormat(r *http.Request) WireFormat {
	if r == nil {
		return WireJSON
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return WireJSON
	}
	// Track the best-weighted recognised candidate per wire format,
	// then compare the two. Scan comma-separated entries in place —
	// `strings.Split` would allocate a slice on every response (it
	// showed up as ~14% of the round-trip's allocations in profiling).
	var best [2]acceptCand // indexed by WireFormat
	idx := 0
	for len(accept) > 0 {
		entry := accept
		if i := strings.IndexByte(accept, ','); i >= 0 {
			entry, accept = accept[:i], accept[i+1:]
		} else {
			accept = ""
		}
		mt, q, ok := parseAcceptEntry(entry)
		if !ok || q <= 0 {
			// Unparseable media type or `q=0` (explicitly not
			// acceptable) — skip, but still advance the position
			// counter so list-order tiebreaks stay stable.
			idx++
			continue
		}
		var wf WireFormat
		switch {
		case isProtobufMedia(mt):
			wf = WireProto
		case mt == MIMEJSON || mt == "*/*" || mt == "application/*":
			wf = WireJSON
		default:
			idx++
			continue
		}
		cand := acceptCand{set: true, q: q, spec: mediaSpecificity(mt), idx: idx}
		if cand.better(best[wf]) {
			best[wf] = cand
		}
		idx++
	}

	if best[WireProto].better(best[WireJSON]) {
		return WireProto
	}
	if best[WireJSON].set {
		return WireJSON
	}
	if best[WireProto].set {
		return WireProto
	}
	return WireJSON
}

// acceptCand is the best Accept entry seen so far for one wire
// format: its quality weight, media-type specificity, and the
// list position it appeared at (for stable tiebreaks).
type acceptCand struct {
	set  bool
	q    float64
	spec int
	idx  int
}

// better reports whether c strictly outranks o under the
// content-negotiation ordering: higher q, then higher
// specificity, then earlier list position. An unset candidate
// never outranks anything; any set candidate outranks an unset
// one.
func (c acceptCand) better(o acceptCand) bool {
	if !c.set {
		return false
	}
	if !o.set {
		return true
	}
	if c.q != o.q {
		return c.q > o.q
	}
	if c.spec != o.spec {
		return c.spec > o.spec
	}
	return c.idx < o.idx
}

// mediaSpecificity ranks a bare media type for tiebreaking:
// `*/*` (0) < `type/*` (1) < fully specified `type/subtype` (2).
func mediaSpecificity(mt string) int {
	switch {
	case mt == "*/*":
		return 0
	case strings.HasSuffix(mt, "/*"):
		return 1
	default:
		return 2
	}
}

// parseAcceptEntry splits one Accept list entry into its bare,
// lower-cased media type and its `q` weight (default 1.0 when the
// parameter is absent or malformed). ok is false for an empty
// media type. Non-q parameters (charset, level, …) are ignored —
// the gateway negotiates only the media type.
func parseAcceptEntry(entry string) (mt string, q float64, ok bool) {
	q = 1.0
	first := true
	for len(entry) > 0 {
		part := entry
		if i := strings.IndexByte(entry, ';'); i >= 0 {
			part, entry = entry[:i], entry[i+1:]
		} else {
			entry = ""
		}
		part = strings.TrimSpace(part)
		if first {
			mt = strings.ToLower(part)
			first = false
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(part[:eq]), "q") {
			if f, err := strconv.ParseFloat(strings.TrimSpace(part[eq+1:]), 64); err == nil {
				q = f
			}
		}
	}
	if mt == "" {
		return "", 0, false
	}
	return mt, q, true
}

// mediaType extracts the bare media type from one header value,
// dropping any `;`-delimited parameters (charset, q-value) and
// surrounding whitespace, lower-cased for comparison.
func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// isProtobufMedia reports whether a bare media type is one of the
// recognised protobuf spellings.
func isProtobufMedia(mt string) bool {
	return mt == MIMEProtobuf || mt == MIMEProtobufXAlt
}
