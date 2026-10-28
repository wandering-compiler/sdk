package egress

import (
	"context"
	"net/http"
	"sync"
)

// RequestHook runs on every attempt's request, after the client set the
// credential, before it is sent: the place for a signature over the request,
// which a retry must recompute. An error ends the call (INTERNAL).
type RequestHook func(ctx context.Context, r *http.Request) error

// ResponseHook runs on every attempt's response before its body is read. An
// error ends the call (INTERNAL).
type ResponseHook func(ctx context.Context, r *http.Response) error

type hookSet[F any] struct {
	mu sync.RWMutex
	// by client, then method ("" = every method of the client)
	m map[string]map[string][]F
}

var (
	before hookSet[RequestHook]
	after  hookSet[ResponseHook]
)

// BeforeRequest registers f for the calls of client (its egress name, e.g.
// "stripe") — every method when method is "", else only the named proto
// method ("CreatePaymentIntent"). Hooks run in registration order; register
// them before the first call.
func BeforeRequest(client, method string, f RequestHook) { before.add(client, method, f) }

// AfterResponse registers f the way BeforeRequest does.
func AfterResponse(client, method string, f ResponseHook) { after.add(client, method, f) }

func (h *hookSet[F]) add(client, method string, f F) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]map[string][]F{}
	}
	if h.m[client] == nil {
		h.m[client] = map[string][]F{}
	}
	h.m[client][method] = append(h.m[client][method], f)
}

// hooksFor is a call's hooks: the client-wide ones, then the method's.
func hooksFor[F any](h *hookSet[F], m *method) []F {
	h.mu.RLock()
	defer h.mu.RUnlock()
	byMethod := h.m[m.client]
	if byMethod == nil {
		return nil
	}
	out := append([]F(nil), byMethod[""]...)
	return append(out, byMethod[m.name]...)
}
