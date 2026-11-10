package eventlog

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// HTTP surfaces (REST, admin): the handler knows the method, the request
// and the result, the middleware around it knows the status and the
// client. [Surface.HTTPMiddleware] gives each request a slot holding what
// the request says about its client; the handler's first line, [Begin] (or
// the first recorded call through [Surface.Conn]), starts the record in it
// with those facts already set; the middleware finishes it once the handler
// has returned.
//
// The client facts go in when the record begins, not when the middleware
// finishes it: a sync audit record is finished inside the handler, before
// the response, and handed to the batcher there — anything written to it
// afterwards was lost to the stored record and raced the batcher's read.

type slotKey struct{}

type slot struct {
	rec *Record
	// earlier holds the records of earlier Begin calls in this request; the
	// middleware finishes them with the request's status, as it does rec.
	earlier []*Record

	// What the request says about its client, read before the handler ran.
	ip, userAgent, referrer string
	utm                     map[string]string
	requestID               func(context.Context) string
}

// begin puts rec in the slot and gives it the request's client and its
// request id, read from ctx — the handler's, so a request id set by any
// middleware between this one and the handler is seen.
func (sl *slot) begin(ctx context.Context, rec *Record) {
	if sl.rec != nil {
		sl.earlier = append(sl.earlier, sl.rec)
	}
	sl.rec = rec
	rec.SetClient(sl.ip, sl.userAgent, sl.referrer, sl.utm)
	if sl.requestID != nil {
		rec.SetRequestID(sl.requestID(ctx))
	}
}

// HTTPMiddleware gives the record a handler begins for the request the
// client (through trustedProxies, as the rate limiter reads it) and the
// request id (requestID may be nil), and finishes it with the status
// written. A nil Surface returns next unchanged.
func (s *Surface) HTTPMiddleware(trustedProxies []netip.Prefix, requestID func(context.Context) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if s == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sl := &slot{
				ip:        clientIP(r, trustedProxies),
				userAgent: r.UserAgent(),
				referrer:  r.Referer(),
				requestID: requestID,
			}
			if s.purposes.Marketing {
				sl.utm = utmOf(r)
			}
			sw := &statusWriter{ResponseWriter: w}
			ctx := context.WithValue(r.Context(), slotKey{}, sl)
			// A handler that panics after Begin is still recorded: the
			// deferred finish runs while the panic unwinds through here,
			// and the panic goes on unrecovered to whatever answers it.
			returned := false
			defer func() {
				st := sw.status
				switch {
				case st != 0:
				case returned:
					st = http.StatusOK
				default:
					st = http.StatusInternalServerError
				}
				for _, rec := range sl.earlier {
					rec.Finish(ctx, st)
				}
				if sl.rec != nil { // nil: no recorded method served it
					sl.rec.Finish(ctx, st)
				}
			}()
			next.ServeHTTP(sw, r.WithContext(ctx))
			returned = true
		})
	}
}

// Begin starts the record of the request ctx carries, for m; nil (records
// nothing) when the surface is off or no middleware gave the request a slot.
func Begin(ctx context.Context, s *Surface, m *Method) *Record {
	if s == nil {
		return nil
	}
	sl, _ := ctx.Value(slotKey{}).(*slot)
	if sl == nil {
		return nil
	}
	sl.begin(ctx, s.Start(m))
	return sl.rec
}

func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	return ratelimit.ClientIP(peer, r.Header.Values("X-Forwarded-For"), trusted).Unmap().String()
}

func utmOf(r *http.Request) map[string]string {
	if !strings.Contains(r.URL.RawQuery, "utm_") {
		return nil
	}
	var out map[string]string
	for k, v := range r.URL.Query() {
		if name, ok := strings.CutPrefix(k, "utm_"); ok && name != "" && len(v) > 0 {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = v[0]
		}
	}
	return out
}

// statusWriter remembers the status and keeps what the writer it wraps can
// do: flushing (server-sent events), hijacking (WebSocket), and Unwrap for
// http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("eventlog: the response writer cannot be hijacked")
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return h.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
