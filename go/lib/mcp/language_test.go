package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

func registerCS(t *testing.T, msgid, msgstr string) {
	t.Helper()
	t.Cleanup(i18n.Reset)
	po := "msgid \"\"\nmsgstr \"Content-Type: text/plain; charset=UTF-8\\n\"\n\nmsgid \"" + msgid + "\"\nmsgstr \"" + msgstr + "\"\n"
	if err := i18n.RegisterCatalog("cs", []byte(po)); err != nil {
		t.Fatal(err)
	}
}

// The connection's language: the HTTP header on HTTP, an initialize
// parameter or W17_LANGUAGE on stdio.
func TestRequestLanguage(t *testing.T) {
	h := http.Header{}
	h.Set("Accept-Language", "cs-CZ,cs;q=0.9")
	for _, c := range []struct {
		conn ConnectionInfo
		want string
	}{
		{ConnectionInfo{HTTPHeaders: h}, "cs-CZ,cs;q=0.9"},
		{ConnectionInfo{HTTPHeaders: http.Header{}, Env: map[string]string{"W17_LANGUAGE": "de"}}, ""},
		{ConnectionInfo{InitializeParams: map[string]any{"accept-language": "sk"}}, "sk"},
		{ConnectionInfo{Env: map[string]string{"W17_LANGUAGE": "de"}}, "de"},
	} {
		if got := requestLanguage(c.conn); got != c.want {
			t.Errorf("requestLanguage(%+v) = %q, want %q", c.conn, got, c.want)
		}
	}
}

// The Origin guard's refusal is the catalog sentence in the request's
// language, not an English literal.
func TestOriginGuard_Translated(t *testing.T) {
	registerCS(t, MsgidForbiddenOrigin, "Tento web nesmí používat tento server.")
	h := originGuard(map[string]bool{"https://ok.example": true}, http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Accept-Language", "cs")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Tento web nesmí") {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
}

// A tool error with no sentence (a recovered panic) gets its code's catalog
// sentence in the caller's language.
func TestToolErrorResult_EmptySentenceIsTranslated(t *testing.T) {
	ctx := withLanguage(context.Background(), "cs")
	r := toolErrorResult(ctx, status.Error(codes.Internal, ""))
	b := strings.TrimSpace(r.Content[0].(mcp.TextContent).Text)
	if !strings.HasPrefix(b, "Internal: ") || len(b) <= len("Internal: ") {
		t.Errorf("text = %q", b)
	}
}

func TestVocabulary(t *testing.T) {
	v := Vocabulary()
	if len(v) != 2 || v[0] != MsgidAuthUnavailable || v[1] != MsgidForbiddenOrigin {
		t.Errorf("Vocabulary() = %v", v)
	}
}

// A tool call carries the HTTP request's language to the backend, and
// answers its own refusals in it.
func TestCallUnary_CarriesTheLanguage(t *testing.T) {
	s := NewServer("t", "1", nil)
	req := mcp.CallToolRequest{}
	req.Params.Name = "x"
	h := http.Header{}
	h.Set("Accept-Language", "cs")
	ctx := context.WithValue(context.Background(), httpHeadersCtxKey{}, h)
	var seen string
	_, err := s.CallUnary(ctx, req, &emptypb.Empty{}, func(ctx context.Context, _ proto.Message) (proto.Message, error) {
		seen = i18n.LocaleFromContext(ctx)
		return &emptypb.Empty{}, nil
	})
	if err != nil || seen != "cs" {
		t.Errorf("backend saw language %q (err %v), want cs", seen, err)
	}
}
