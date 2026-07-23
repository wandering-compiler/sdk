package stress

import (
	"strings"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// LoadCaller fires load requests over a tuned fasthttp client so the
// runner is not the bottleneck — fasthttp's zero-alloc request path keeps
// client CPU off the gateway-under-test even when co-located, and a large
// per-host connection pool avoids net/http's classic load failure modes
// (default MaxIdleConnsPerHost=2 → connection re-dial storm; HTTP/2 single
// connection → stream-limit serialization). Routing is shared with the
// asserting RESTCaller via runner.ResolveREST, so a load op maps to
// exactly the HTTP call e2e would make. Only the REST (and REST-shaped
// admin) transport is driven under load.
type LoadCaller struct {
	BaseURL string
	timeout time.Duration
	client  *fasthttp.Client
}

// NewLoadCaller builds a load caller against baseURL with a connection
// pool sized to the worker count (a tiny pool is the usual bottleneck).
func NewLoadCaller(baseURL string, concurrency int, timeout time.Duration) *LoadCaller {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxConns := concurrency * 2
	if maxConns < 512 {
		maxConns = 512
	}
	return &LoadCaller{
		BaseURL: strings.TrimRight(baseURL, "/"),
		timeout: timeout,
		client: &fasthttp.Client{
			MaxConnsPerHost:     maxConns,
			MaxIdleConnDuration: 90 * time.Second,
			ReadTimeout:         timeout,
			WriteTimeout:        timeout,
		},
	}
}

// Fire issues one load request and returns its HTTP status (or -1 on a
// transport/connection error, with the error). It drains+releases the
// response so the connection is reused; the body is discarded — load
// measures status + latency, not response contracts.
func (c *LoadCaller) Fire(ep runner.Endpoint, input map[string]any, token string, headers map[string]string) (int, error) {
	method, target, body, err := runner.ResolveREST(c.BaseURL, ep, input)
	if err != nil {
		return -1, err
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(target)
	req.Header.SetMethod(method)
	if body != nil {
		req.Header.SetContentType("application/json")
		req.SetBody(body)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Static per-op headers, after the built-ins so a preset can't
	// silently override Authorization (mirrors RESTCaller).
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		req.Header.Set(k, v)
	}

	if err := c.client.DoTimeout(req, resp, c.timeout); err != nil {
		return -1, err
	}
	return resp.StatusCode(), nil
}
