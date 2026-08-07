package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Caller issues one call to the target and returns the response as a
// decoded, string-keyed object the matcher layer asserts against.
// `token` is the resolved auth credential ("" when the endpoint needs
// none); the caller wires it into the transport — the test file never
// carries an Authorization header.
type Caller interface {
	// files carries multipart file parts; empty means an ordinary
	// JSON call. It is on the INTERFACE rather than on an optional
	// side-channel so every transport has to answer for it — MCP has
	// no multipart, and saying so out loud beats silently dropping the
	// files and asserting against an upload that never happened.
	Call(ctx context.Context, ep Endpoint, input map[string]any, token string, headers map[string]string, files []FilePart) (map[string]any, error)
}

// RESTCaller drives the REST transport over HTTP. It reconstructs the
// gateway's own routing from the baked endpoint table: path
// placeholders and query params are pulled from the input, the
// remainder is the JSON body.
type RESTCaller struct {
	BaseURL string
	Client  *http.Client
}

// NewRESTCaller builds a REST caller against baseURL (e.g.
// "http://localhost:8080"). A nil client uses http.DefaultClient.
func NewRESTCaller(baseURL string, client *http.Client) *RESTCaller {
	if client == nil {
		client = http.DefaultClient
	}
	return &RESTCaller{BaseURL: strings.TrimRight(baseURL, "/"), Client: client}
}

// ResolveREST turns a baked REST endpoint + expanded input into the
// concrete (method, absolute target URL, JSON request body) the transport
// issues. It reconstructs the gateway's own routing: path placeholders and
// query params are pulled from the input, and the remainder is the JSON
// body (a GET/DELETE carries no body, so every remaining scalar becomes a
// query param — covering cursor-paging knobs the table doesn't enumerate).
// baseURL is prefixed onto the path (already trimmed of a trailing slash
// by the caller); pass "" for a path-relative target. body is nil for
// bodyless verbs / empty bodies. It is the SINGLE routing implementation,
// shared by the asserting RESTCaller and the stress LoadCaller, so the two
// can never drift on how a step maps to an HTTP call.
func ResolveREST(baseURL string, ep Endpoint, input map[string]any) (method, target string, body []byte, err error) {
	// Work on a shallow copy so path/query extraction doesn't mutate
	// the caller's map.
	rest := make(map[string]any, len(input))
	for k, v := range input {
		rest[k] = v
	}

	path := ep.PathTemplate
	for _, p := range ep.PathParams {
		v, ok := rest[p]
		if !ok {
			return "", "", nil, fmt.Errorf("rest %s: path param %q missing from input", ep.Ref, p)
		}
		path = strings.ReplaceAll(path, "{"+p+"}", url.PathEscape(fmt.Sprint(v)))
		delete(rest, p)
	}

	q := url.Values{}
	for _, name := range ep.QueryParams {
		if v, ok := rest[name]; ok {
			q.Set(name, fmt.Sprint(v))
			delete(rest, name)
		}
	}

	// A GET / DELETE carries no body, so every remaining scalar input
	// belongs in the query string — this covers params the routing table
	// doesn't enumerate as explicit bindings, notably cursor-paging knobs
	// (`?limit=`, `?cursor=`) the gateway reads straight off the URL.
	// Nested objects can't be a flat query value (iter-1: the author
	// omits or flattens them); repeated scalars become repeated keys.
	method = strings.ToUpper(ep.HTTPMethod)
	if method == http.MethodGet || method == http.MethodDelete {
		for k, v := range rest {
			switch vv := v.(type) {
			case map[string]any:
				continue
			case []any:
				for _, e := range vv {
					q.Add(k, fmt.Sprint(e))
				}
			default:
				q.Set(k, fmt.Sprint(v))
			}
			delete(rest, k)
		}
	}

	target = baseURL + path
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}

	// A raw-body endpoint takes its body VERBATIM: the named input value
	// is the bytes on the wire, with no JSON envelope around it. That is
	// the whole contract — the handler hashes / MACs exactly what the
	// sender wrote — so encoding it as `{"payload": "…"}` here would let
	// a step assert a digest of the runner's own envelope and call that
	// evidence. Every other input field is header- / path- / query-bound
	// by declaration (a raw-body endpoint has no body left for them), so
	// the remainder is deliberately not appended anywhere.
	if ep.RawBodyField != "" {
		v, ok := input[ep.RawBodyField]
		if !ok {
			return "", "", nil, fmt.Errorf("rest %s: raw body field %q missing from input", ep.Ref, ep.RawBodyField)
		}
		switch vv := v.(type) {
		case string:
			body = []byte(vv)
		case []byte:
			body = vv
		default:
			return "", "", nil, fmt.Errorf("rest %s: raw body field %q must be a string, got %T", ep.Ref, ep.RawBodyField, v)
		}
		return method, target, body, nil
	}

	var payload any = rest
	if ep.BodyField != "" && ep.BodyField != "*" {
		payload = input[ep.BodyField]
	}
	if method != http.MethodGet && method != http.MethodDelete && len(rest) > 0 {
		b, err := json.Marshal(payload)
		if err != nil {
			return "", "", nil, fmt.Errorf("rest %s: marshal body: %w", ep.Ref, err)
		}
		body = b
	}
	return method, target, body, nil
}

func (c *RESTCaller) Call(ctx context.Context, ep Endpoint, input map[string]any, token string, headers map[string]string, files []FilePart) (map[string]any, error) {
	method, target, body, err := ResolveREST(c.BaseURL, ep, input)
	if err != nil {
		return nil, err
	}

	var reqBody io.Reader
	contentType := ""
	if len(files) > 0 {
		// A multipart endpoint takes its scalars as ordinary form
		// fields, not as a JSON body — so the JSON `body` computed
		// above is discarded and the same input is re-encoded here.
		// The endpoint's own path/query bindings were already
		// consumed by ResolveREST, so what remains in `input` is the
		// message body's fields.
		mb, ct, err := multipartBody(ep, input, files)
		if err != nil {
			return nil, err
		}
		reqBody, contentType = bytes.NewReader(mb), ct
	} else if body != nil {
		reqBody, contentType = bytes.NewReader(body), "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reqBody)
	if err != nil {
		return nil, fmt.Errorf("rest %s: build request: %w", ep.Ref, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Static per-step headers (e.g. X-Device-Id). Set after the
	// built-ins so a scenario can't silently override Authorization.
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rest %s %s: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, newCallError(fmt.Sprintf("rest %s %s", method, target), resp.StatusCode, raw)
	}
	return decodeWithWire(raw, resp.Header.Get("Content-Type"))
}

// WireKey is the synthetic response field carrying the WIRE facts of an
// HTTP response: the media type the surface actually chose, the payload
// size, and whether the bytes are JSON at all. It is injected alongside
// the decoded fields (never overwriting a real one), so a test file
// asserts it like any other field:
//
//	headers: {Accept: application/protobuf}
//	expect:
//	    w17_wire.content_type: {matcher: eq, value: application/protobuf}
//	    w17_wire.json: {matcher: eq, value: false}
//
// It exists because the runner carries no proto descriptors: a binary
// protobuf body is opaque to it, so without these facts a scenario could
// never assert that content negotiation (C9) actually happened — the
// step would just fail to decode. A non-JSON body therefore yields ONLY
// this field; there is nothing else the runner can honestly report.
const WireKey = "w17_wire"

// decodeWithWire decodes a response body and attaches WireKey.
//
// Only a BINARY media type skips the JSON decode. Everything else
// decodes exactly as it did before this field existed — including a
// body some server left unlabelled or mislabelled `text/plain`, which
// net/http does by default when a handler writes JSON without setting
// the header. Sniffing the bytes instead would be worse in the case
// that matters: a JSON body that is malformed must keep failing loudly
// (a silent downgrade to "no fields" makes every matcher report the
// useless "field absent" instead of the parse error).
//
// The `json` wire fact is computed from the BYTES, independent of the
// label, so a step can assert what the payload really is.
func decodeWithWire(raw []byte, contentType string) (map[string]any, error) {
	mt := mediaType(contentType)
	trimmed := bytes.TrimSpace(raw)
	wire := map[string]any{
		"content_type": mt,
		"size":         float64(len(raw)),
		"json":         len(trimmed) > 0 && json.Valid(trimmed),
	}
	if isBinaryMedia(mt) {
		return map[string]any{WireKey: wire}, nil
	}
	out, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	if _, taken := out[WireKey]; !taken {
		out[WireKey] = wire
	}
	return out, nil
}

// isBinaryMedia reports whether a response media type is one the runner
// cannot decode into fields: the protobuf wire (both spellings — see
// restgw's MIMEProtobuf / MIMEProtobufXAlt) and the generic binary type.
func isBinaryMedia(mt string) bool {
	switch mt {
	case "application/protobuf", "application/x-protobuf", "application/octet-stream":
		return true
	}
	return false
}

// mediaType strips the parameters off a Content-Type
// ("application/json; charset=utf-8" → "application/json") and
// lowercases it, so an `eq` matcher can name the bare media type.
func mediaType(ct string) string {
	base, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// decodeObject parses a JSON response body into a string-keyed map.
// An empty body decodes to an empty object (so a 204-style response
// still matches an empty `expect`).
func decodeObject(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, truncate(raw, 256))
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// CallError is a surface's REFUSAL, decoded rather than stringified.
//
// Every generated surface writes the same envelope —
// `{"error":{"code","message"}}`, where `code` is the canonical gRPC
// code name — so a scenario can assert WHICH guard fired instead of
// only that the call did not succeed. Before this existed the runner
// collapsed any status >= 400 into a plain formatted error, which made
// a refusal indistinguishable from a typo in the request and left the
// deny direction of every authorization gate unassertable.
//
// Status is retained for the message only. Assertions name the code:
// the test files are gRPC-shaped and deliberately carry no transport.
type CallError struct {
	Op      string // "rest GET /api/v1/…", for the message
	Status  int
	Code    string // canonical code from the envelope, "" if absent
	Message string // human message from the envelope, "" if absent
	Raw     string // the body, truncated — the fallback when it is not our envelope

	// Details are the envelope's per-field entries, empty for a
	// refusal that carries none. A whole class of refusals answers the
	// SAME `message` — every request-validation failure says
	// "validation failed" — and names the rule that fired only here,
	// so dropping them leaves six different rules indistinguishable.
	Details []CallErrorDetail
}

// CallErrorDetail is one entry of a refusal's `details[]`: which field
// the surface objected to, under which rule, and what it said.
type CallErrorDetail struct {
	Field   string
	Code    string
	Message string
}

func (e *CallError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: status %d: %s: %s", e.Op, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: status %d: %s", e.Op, e.Status, e.Raw)
}

// newCallError decodes the canonical envelope out of an error response.
// A body that is not that envelope (a proxy's HTML, a panic page) leaves
// Code empty and keeps the raw text, so the failure still reads — an
// assertion against it then fails on the MISSING code, which is the
// honest outcome: the surface did not answer in the contract.
func newCallError(op string, status int, raw []byte) *CallError {
	e := &CallError{Op: op, Status: status, Raw: truncate(raw, 512)}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Field   string `json:"field"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		e.Code = env.Error.Code
		e.Message = env.Error.Message
		for _, d := range env.Error.Details {
			e.Details = append(e.Details, CallErrorDetail{Field: d.Field, Code: d.Code, Message: d.Message})
		}
	}
	return e
}

// multipartBody encodes a step's call as `multipart/form-data`: every
// remaining input scalar as an ordinary form field, every FilePart as a
// file part.
//
// Scalars go in as form fields rather than as a nested JSON blob because
// that is what the gateway's multipart decoder reads. Non-scalars are
// refused rather than silently JSON-encoded: a message-valued field in a
// multipart request has no agreed encoding, and guessing one here would
// produce a request the gateway parses differently from how this file
// says it was sent.
func multipartBody(ep Endpoint, input map[string]any, files []FilePart) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// Deterministic field order — a multipart body that reorders between
	// runs makes a captured failure impossible to compare with a rerun.
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		switch v := input[k].(type) {
		case nil:
			continue
		case string:
			if err := w.WriteField(k, v); err != nil {
				return nil, "", fmt.Errorf("rest %s: multipart field %q: %w", ep.Ref, k, err)
			}
		case bool, int, int32, int64, uint, uint32, uint64, float32, float64, json.Number:
			if err := w.WriteField(k, fmt.Sprint(v)); err != nil {
				return nil, "", fmt.Errorf("rest %s: multipart field %q: %w", ep.Ref, k, err)
			}
		default:
			return nil, "", fmt.Errorf("rest %s: multipart field %q is %T — a multipart request carries scalars as form fields, and there is no agreed encoding for a nested value here", ep.Ref, k, v)
		}
	}

	for _, f := range files {
		part, err := w.CreateFormFile(f.Part, f.Filename)
		if err != nil {
			return nil, "", fmt.Errorf("rest %s: multipart file part %q: %w", ep.Ref, f.Part, err)
		}
		if _, err := io.WriteString(part, f.Content); err != nil {
			return nil, "", fmt.Errorf("rest %s: multipart file part %q: write: %w", ep.Ref, f.Part, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("rest %s: multipart close: %w", ep.Ref, err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}
