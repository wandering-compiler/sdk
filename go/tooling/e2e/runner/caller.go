package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Caller issues one call to the target and returns the response as a
// decoded, string-keyed object the matcher layer asserts against.
// `token` is the resolved auth credential ("" when the endpoint needs
// none); the caller wires it into the transport — the test file never
// carries an Authorization header.
type Caller interface {
	Call(ctx context.Context, ep Endpoint, input map[string]any, token string, headers map[string]string) (map[string]any, error)
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

func (c *RESTCaller) Call(ctx context.Context, ep Endpoint, input map[string]any, token string, headers map[string]string) (map[string]any, error) {
	method, target, body, err := ResolveREST(c.BaseURL, ep, input)
	if err != nil {
		return nil, err
	}

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reqBody)
	if err != nil {
		return nil, fmt.Errorf("rest %s: build request: %w", ep.Ref, err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
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
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("rest %s %s: status %d: %s", method, target, resp.StatusCode, truncate(raw, 512))
	}
	return decodeObject(raw)
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
