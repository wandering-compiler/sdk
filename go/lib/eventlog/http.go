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
// client. [Surface.HTTPMiddleware] gives each request a slot; the handler's
// first line, [Begin], starts the record in it; the middleware finishes it
// once the handler has returned.

type slotKey struct{}

type slot struct{ rec *Record }

// HTTPMiddleware finishes the record a handler began for the request: the
// status written, the client (through trustedProxies, as the rate limiter
// reads it), the request id (requestID may be nil). A nil Surface returns
// next unchanged.
func (s *Surface) HTTPMiddleware(trustedProxies []netip.Prefix, requestID func(context.Context) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if s == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sl := &slot{}
			sw := &statusWriter{ResponseWriter: w}
			ctx := context.WithValue(r.Context(), slotKey{}, sl)
			next.ServeHTTP(sw, r.WithContext(ctx))
			rec := sl.rec
			if rec == nil {
				return // no recorded method served it
			}
			var utm map[string]string
			if s.purposes.Marketing {
				utm = utmOf(r)
			}
			rec.SetClient(clientIP(r, trustedProxies), r.UserAgent(), r.Referer(), utm)
			if requestID != nil {
				rec.SetRequestID(requestID(ctx))
			}
			st := sw.status
			if st == 0 {
				st = http.StatusOK
			}
			rec.Finish(ctx, st)
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
	sl.rec = s.Start(m)
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
