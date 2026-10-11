package mcp

import (
	"net/http"
	"os"
	"strconv"
)

// DefaultMaxRequestBodyBytes is the POST body cap on the HTTP transport when
// neither SetMaxRequestBodyBytes nor W17_MCP_MAX_BODY_BYTES sets one. It
// matches the REST gateway's default request cap.
const DefaultMaxRequestBodyBytes int64 = 10 << 20

// MaxBodyBytesEnv overrides the default cap (a positive byte count) for a
// server whose cap was not set in code.
const MaxBodyBytesEnv = "W17_MCP_MAX_BODY_BYTES"

// SetMaxRequestBodyBytes caps the size of a request body the HTTP transport
// reads. n <= 0 restores the default. Must be called before
// ServeStreamableHTTP.
//
// mcp-go reads the whole POST body into memory before parsing it, so without
// a cap one request can make the process allocate as much as the client
// sends.
func (s *Server) SetMaxRequestBodyBytes(n int64) { s.maxBody = n }

func (s *Server) maxBodyBytes() int64 {
	if s.maxBody > 0 {
		return s.maxBody
	}
	if v, err := strconv.ParseInt(os.Getenv(MaxBodyBytesEnv), 10, 64); err == nil && v > 0 {
		return v
	}
	return DefaultMaxRequestBodyBytes
}

// limitBody bounds POST bodies. A declared Content-Length over the cap is
// refused up front with 413 and a JSON-RPC error; a body without one (chunked)
// is wrapped in http.MaxBytesReader, so the read stops at the cap and mcp-go
// answers with its body-read error instead of buffering the rest.
func limitBody(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > limit {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"request body too large"}}`))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}
