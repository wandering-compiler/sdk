package mcp

import (
	"context"
	"net/http"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// requestLanguage is the language a connection asked for: the HTTP
// request's Accept-Language, else an initialize parameter (`accept-language`
// or `w17-language`) or the stdio server's W17_LANGUAGE. "" = none: the
// surface's default applies.
func requestLanguage(conn ConnectionInfo) string {
	if v := requestLanguageFromHeaders(conn.HTTPHeaders); v != "" {
		return v
	}
	if conn.HTTPHeaders != nil {
		return "" // HTTP: the request is the only source
	}
	for _, k := range []string{"accept-language", i18n.LanguageMetadataKey} {
		if v, ok := conn.InitializeParams[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return conn.Env["W17_LANGUAGE"]
}

func requestLanguageFromHeaders(h http.Header) string {
	if h == nil {
		return ""
	}
	return strings.TrimSpace(h.Get("Accept-Language"))
}

// withLanguage carries lang where i18n.T reads it (the outgoing metadata
// the backend call also sends). An empty lang changes nothing.
func withLanguage(ctx context.Context, lang string) context.Context {
	if lang == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, i18n.LanguageMetadataKey, lang)
}

// requestContext is an HTTP request's context with its language, for a
// refusal written before any tool call.
func requestContext(r *http.Request) context.Context {
	return withLanguage(r.Context(), requestLanguageFromHeaders(r.Header))
}
