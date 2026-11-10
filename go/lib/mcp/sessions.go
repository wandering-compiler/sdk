package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// The streamable HTTP transport keeps a session for every `initialize` (and
// for every notification stream a GET opens) until the client sends DELETE.
// Nothing makes a client send it — a crashed client, a reconnecting one or a
// hostile one simply does not — and each session holds about 10 KiB of
// channels and maps, so without a bound a stream of ~200-byte POSTs grows the
// process until it is restarted. Three bounds close that: a session idle for
// the TTL is ended, and the live sessions are capped in total and per client
// address.
//
// The idle sweep is this package's, not mcp-go's WithSessionIdleTTL: mcp-go's
// records the last activity of every session id a request names, live or not
// — a key per forged id until the sweep — while this one tracks only the
// sessions it counted, and ends one the way a client would, with a DELETE
// through the transport, which drops all of its state.

const (
	// DefaultSessionIdleTTL is how long a session may go without a request
	// (and without an open notification stream) before it is ended. A
	// client that comes back later initializes again.
	DefaultSessionIdleTTL = 30 * time.Minute
	// DefaultMaxSessions caps the live sessions of one process (~100 MiB).
	DefaultMaxSessions = 10000
	// DefaultMaxSessionsPerAddress caps the live sessions of one client
	// address, so one address cannot take the whole of DefaultMaxSessions.
	DefaultMaxSessionsPerAddress = 100
)

// Environment overrides of the session bounds. Only a positive whole number
// is read; any other value keeps the default — a bound cannot be switched off.
const (
	SessionIdleSecondsEnv    = "W17_MCP_SESSION_IDLE_SECONDS"
	MaxSessionsEnv           = "W17_MCP_MAX_SESSIONS"
	MaxSessionsPerAddressEnv = "W17_MCP_MAX_SESSIONS_PER_ADDRESS"
)

// SessionLimits bounds the sessions of the streamable HTTP transport. A zero
// field takes its environment override, else its default.
type SessionLimits struct {
	IdleTTL       time.Duration
	Max           int
	MaxPerAddress int
}

// SetSessionLimits sets the session bounds of the HTTP transport. Must be
// called before ServeStreamableHTTP.
func (s *Server) SetSessionLimits(l SessionLimits) { s.sessionLimits = l }

func (s *Server) resolvedSessionLimits() SessionLimits {
	l := s.sessionLimits
	if l.IdleTTL <= 0 {
		l.IdleTTL = time.Duration(envPositive(SessionIdleSecondsEnv, int(DefaultSessionIdleTTL/time.Second))) * time.Second
	}
	if l.Max <= 0 {
		l.Max = envPositive(MaxSessionsEnv, DefaultMaxSessions)
	}
	if l.MaxPerAddress <= 0 {
		l.MaxPerAddress = envPositive(MaxSessionsPerAddressEnv, DefaultMaxSessionsPerAddress)
	}
	return l
}

func envPositive(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// sessionGuard counts the transport's registered sessions per client address,
// refuses a request that would register one more past a cap, and ends the
// sessions that went idle.
//
// A request that may register a session (an `initialize` POST, a GET for a
// session not yet live) reserves a slot before it reaches mcp-go; the
// session-registration hook turns the reservation into a live session, and a
// reservation the request did not use is given back when it returns. A
// session leaves the count when mcp-go unregisters it — on DELETE (the
// client's or the sweep's) or at the end of the GET stream that opened it.
type sessionGuard struct {
	mu           sync.Mutex
	max, perAddr int
	total        int            // live sessions + open reservations
	byAddr       map[string]int // the same, per address
	live         map[string]*liveSession
	now          func() time.Time
}

type liveSession struct {
	addr    string
	last    time.Time
	streams int // notification streams (GETs) open on it
}

func newSessionGuard() *sessionGuard {
	return &sessionGuard{byAddr: map[string]int{}, live: map[string]*liveSession{}, now: time.Now}
}

type sessionReservation struct {
	addr   string
	stream bool   // a GET: the session lives while the request runs
	id     string // set when a session took it
	used   bool
}

type reservationCtxKey struct{}

// reserve takes a slot for addr; ok=false when a cap is reached, perAddr
// telling which one.
func (g *sessionGuard) reserve(addr string, stream bool) (res *sessionReservation, ok, perAddr bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byAddr[addr] >= g.perAddr {
		return nil, false, true
	}
	if g.total >= g.max {
		return nil, false, false
	}
	g.total++
	g.byAddr[addr]++
	return &sessionReservation{addr: addr, stream: stream}, true, false
}

// settle gives back a reservation no session took, and ends the stream a GET
// session held.
func (g *sessionGuard) settle(res *sessionReservation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !res.used {
		g.releaseLocked(res.addr)
		return
	}
	if ls, ok := g.live[res.id]; ok && res.stream {
		ls.streams--
		ls.last = g.now()
	}
}

func (g *sessionGuard) releaseLocked(addr string) {
	g.total--
	if g.byAddr[addr]--; g.byAddr[addr] <= 0 {
		delete(g.byAddr, addr)
	}
}

// touch records a request on a live session and reports whether it is live.
// A GET (stream) keeps the session busy until done is called.
func (g *sessionGuard) touch(id string, stream bool) (live bool, done func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ls, ok := g.live[id]
	if !ok {
		return false, func() {}
	}
	ls.last = g.now()
	if !stream {
		return true, func() {}
	}
	ls.streams++
	return true, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if ls, ok := g.live[id]; ok {
			ls.streams--
			ls.last = g.now()
		}
	}
}

// registered is mcp-go's session-registration hook: the reservation in ctx
// becomes the session's slot. A session registered without one (a path that
// reserved nothing) is counted too, so the count never drifts below what is
// live.
func (g *sessionGuard) registered(ctx context.Context, session mcpserver.ClientSession) {
	id := session.SessionID()
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dup := g.live[id]; dup {
		return
	}
	ls := &liveSession{last: g.now()}
	if res, ok := ctx.Value(reservationCtxKey{}).(*sessionReservation); ok && !res.used {
		res.used, res.id = true, id
		ls.addr = res.addr
		if res.stream {
			ls.streams = 1
		}
	} else {
		ls.addr, _ = ctx.Value(clientAddrCtxKey{}).(string)
		g.total++
		g.byAddr[ls.addr]++
	}
	g.live[id] = ls
}

// unregistered is mcp-go's session-unregistration hook.
func (g *sessionGuard) unregistered(_ context.Context, session mcpserver.ClientSession) {
	id := session.SessionID()
	g.mu.Lock()
	defer g.mu.Unlock()
	ls, ok := g.live[id]
	if !ok {
		return
	}
	delete(g.live, id)
	g.releaseLocked(ls.addr)
}

// count reports the live sessions and open reservations.
func (g *sessionGuard) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.total
}

// idle lists the sessions with no open stream and no request for ttl.
func (g *sessionGuard) idle(ttl time.Duration) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	var ids []string
	for id, ls := range g.live {
		if ls.streams <= 0 && now.Sub(ls.last) >= ttl {
			ids = append(ids, id)
		}
	}
	return ids
}

// sweep ends every idle session until ctx ends, checking every ttl/2: a
// session is ended at most 1.5 × ttl after its last request. end is a DELETE
// through the transport, so mcp-go drops the session's whole state and the
// unregistration hook frees its slot.
func (g *sessionGuard) sweep(ctx context.Context, ttl time.Duration, end func(id string)) {
	t := time.NewTicker(max(ttl/2, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, id := range g.idle(ttl) {
				end(id)
			}
		}
	}
}

// middleware refuses a session-creating request past a cap: 429 when the
// caller's address holds its share, 503 when the process is full; both with
// Retry-After and a JSON-RPC error body. Requests on a live session pass and
// count as activity.
func (g *sessionGuard) middleware(next http.Handler, addrOf func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream := r.Method == http.MethodGet
		if id := r.Header.Get(mcpserver.HeaderKeySessionID); id != "" {
			if live, done := g.touch(id, stream); live {
				defer done()
				next.ServeHTTP(w, r)
				return
			}
		}
		if !mayCreateSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		res, ok, perAddr := g.reserve(addrOf(r), stream)
		if !ok {
			code, msg := http.StatusServiceUnavailable, "too many MCP sessions on this server"
			if perAddr {
				code, msg = http.StatusTooManyRequests, "too many MCP sessions from this address"
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(code)
			body, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]any{"code": -32000, "message": msg},
			})
			_, _ = w.Write(body)
			return
		}
		defer g.settle(res)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reservationCtxKey{}, res)))
	})
}

// mayCreateSession reports whether mcp-go could register a new session for a
// request not on a live session: an `initialize` POST (decided from the body
// the way mcp-go decides it; the body — already capped by limitBody — is read
// here and put back), or any GET. Everything else keeps no session.
func mayCreateSession(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		return true
	case http.MethodPost:
		if r.Body == nil {
			return false
		}
		body, err := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err != nil {
			// mcp-go answers the read error itself; it cannot parse an
			// initialize out of a body cut short either.
			return false
		}
		var msg struct {
			Method string `json:"method"`
		}
		return json.Unmarshal(body, &msg) == nil && msg.Method == "initialize"
	}
	return false
}

// hook wires g into the server's hooks.
func (g *sessionGuard) hook(h *mcpserver.Hooks) {
	h.AddOnRegisterSession(g.registered)
	h.AddOnUnregisterSession(g.unregistered)
}

// endSession ends one session through the transport, as a client's DELETE
// would.
func endSession(transport http.Handler, id string) {
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, EndpointPath, http.NoBody)
	r.Header.Set(mcpserver.HeaderKeySessionID, id)
	transport.ServeHTTP(discardResponse{h: http.Header{}}, r)
}

// discardResponse is the ResponseWriter of an internal request.
type discardResponse struct{ h http.Header }

func (d discardResponse) Header() http.Header         { return d.h }
func (d discardResponse) Write(b []byte) (int, error) { return len(b), nil }
func (d discardResponse) WriteHeader(int)             {}
