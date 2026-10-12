package restgw_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

type envelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v — body %s", err, rec.Body.String())
	}
	return env
}

// czech registers a catalog for the sentences under test and returns a
// context that asks for it, the way a gateway request carries its locale.
func czech(t *testing.T) context.Context {
	t.Helper()
	po := "msgid \"\"\nmsgstr \"\"\n\"Content-Type: text/plain; charset=UTF-8\\n\"\n\n"
	for id, tr := range map[string]string{
		restgw.MsgidUploadMissing:  "Nebyl přiložen žádný soubor.",
		restgw.MsgidUploadTooLarge: "Soubor je příliš velký.",
		restgw.MsgidUploadBadType:  "Tento typ souboru není přijímán.",
		restgw.ClientSentence(context.Background(), codes.PermissionDenied):  "K tomuto nemáte přístup.",
		restgw.ClientSentence(context.Background(), codes.ResourceExhausted): "Příliš mnoho požadavků.",
		restgw.ClientSentence(context.Background(), codes.Internal):          "Na naší straně se něco pokazilo.",
	} {
		po += fmt.Sprintf("msgid %q\nmsgstr %q\n\n", id, tr)
	}
	i18n.Register("cs", []byte(po))
	t.Cleanup(i18n.Reset)
	return metadata.AppendToOutgoingContext(context.Background(), i18n.LanguageMetadataKey, "cs")
}

// ---- gateway-written envelopes carry catalog sentences only ----

// The scope name is deployment configuration; the client gets the code's
// sentence in its language and the operator gets the name.
func TestWriteMissingScopeCtx_NameReachesTheOperatorOnly(t *testing.T) {
	ctx := czech(t)
	rec := httptest.NewRecorder()
	logged := captureLog(t, func() { restgw.WriteMissingScopeCtx(ctx, rec, "tenant_id") })

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "tenant_id") || strings.Contains(rec.Body.String(), "scope") {
		t.Errorf("the scope name reached the client: %s", rec.Body.String())
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Code != "PERMISSION_DENIED" || env.Error.Message != "K tomuto nemáte přístup." {
		t.Errorf("envelope = %+v, want PERMISSION_DENIED with the translated catalog sentence", env.Error)
	}
	if !strings.Contains(logged, "missing required scope: tenant_id") || !strings.Contains(logged, "refused:") {
		t.Errorf("the operator did not get the scope name on the refusal channel: %q", logged)
	}
}

// Upload refusals name neither the file, the allowlist nor the limit to the
// client; every one of those still reaches the operator.
func TestWriteUploadErrorCtx_RefusalsAreCatalogSentences(t *testing.T) {
	ctx := czech(t)
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
		secrets    []string
	}{
		{"missing", fmt.Errorf("%w: form field %q", restgw.ErrFilePartMissing, "avatar_blob"),
			http.StatusBadRequest, "INVALID_ARGUMENT", "Nebyl přiložen žádný soubor.", []string{"avatar_blob", "multipart", "restgw"}},
		{"too large", fmt.Errorf("%w (limit=%d bytes)", restgw.ErrUploadTooLarge, 5242880),
			http.StatusRequestEntityTooLarge, "RESOURCE_EXHAUSTED", "Soubor je příliš velký.", []string{"5242880", "limit", "restgw"}},
		{"bad extension", fmt.Errorf("%w: filename %q (extension %q not in allowlist %v)", restgw.ErrUploadBadExt, "evil.exe", "exe", []string{"png", "jpg"}),
			http.StatusBadRequest, "INVALID_ARGUMENT", "Tento typ souboru není přijímán.", []string{"evil.exe", "allowlist", "png", "restgw"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			logged := captureLog(t, func() { restgw.WriteUploadErrorCtx(ctx, rec, tc.err) })
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			for _, s := range tc.secrets {
				if strings.Contains(rec.Body.String(), s) {
					t.Errorf("%q reached the client: %s", s, rec.Body.String())
				}
			}
			env := decodeEnvelope(t, rec)
			if env.Error.Code != tc.wantCode || env.Error.Message != tc.wantMsg {
				t.Errorf("envelope = %+v, want %s / %q", env.Error, tc.wantCode, tc.wantMsg)
			}
			if !strings.Contains(logged, tc.err.Error()) {
				t.Errorf("the operator did not get the helper's error %q: %q", tc.err, logged)
			}
		})
	}
	// The context-free form answers in the source locale, same shape.
	rec := httptest.NewRecorder()
	restgw.WriteUploadError(rec, restgw.ErrUploadTooLarge)
	if env := decodeEnvelope(t, rec); env.Error.Message != restgw.MsgidUploadTooLarge {
		t.Errorf("context-free message = %q, want %q", env.Error.Message, restgw.MsgidUploadTooLarge)
	}
}

// The new sentences are harvested, so a project can translate them.
func TestVocabulary_CarriesTheUploadSentences(t *testing.T) {
	have := map[string]bool{}
	for _, m := range restgw.Vocabulary() {
		have[m] = true
	}
	for _, m := range []string{restgw.MsgidUploadMissing, restgw.MsgidUploadTooLarge, restgw.MsgidUploadBadType} {
		if !have[m] {
			t.Errorf("Vocabulary() misses %q — it would never reach a catalog", m)
		}
	}
}

// A refused public caller reads the catalog's sentence in its language.
func TestAdmitPublic_RefusalIsTheCatalogSentence(t *testing.T) {
	ctx := czech(t)
	l := ratelimit.New(ratelimit.Config{PerMinute: 1, Burst: 1})
	r := httptest.NewRequest(http.MethodPost, "/public", nil).WithContext(ctx)
	r.RemoteAddr = "203.0.113.9:4000"
	_, done, ok := restgw.AdmitPublic(l, httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("first call must be admitted")
	}
	done() // nothing written: stays charged

	rec := httptest.NewRecorder()
	if _, _, ok := restgw.AdmitPublic(l, rec, r); ok {
		t.Fatal("drained bucket must refuse")
	}
	env := decodeEnvelope(t, rec)
	if rec.Code != http.StatusTooManyRequests || env.Error.Code != "RESOURCE_EXHAUSTED" || env.Error.Message != "Příliš mnoho požadavků." {
		t.Errorf("refusal = %d %+v, want 429 RESOURCE_EXHAUSTED with the translated sentence", rec.Code, env.Error)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}

	rec = httptest.NewRecorder()
	if restgw.AdmitPublicStream(l, rec, r) {
		t.Fatal("drained bucket must refuse the stream")
	}
	if env := decodeEnvelope(t, rec); env.Error.Message != "Příliš mnoho požadavků." {
		t.Errorf("stream refusal message = %q, want the translated sentence", env.Error.Message)
	}
}

// A redirect target the gateway will not follow is the handler's fault: the
// browser's user reads the generic sentence, the operator reads why.
func TestWriteBrowserRedirect_RefusalIsTheCatalogSentence(t *testing.T) {
	ctx := czech(t)
	r := httptest.NewRequest(http.MethodGet, "/callback", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	logged := captureLog(t, func() { restgw.WriteBrowserRedirect(rec, r, "//evil.example/steal", "/", "", "") })

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Message != "Na naší straně se něco pokazilo." {
		t.Errorf("message = %q, want the translated INTERNAL sentence", env.Error.Message)
	}
	if strings.Contains(rec.Body.String(), "redirect") || strings.Contains(rec.Body.String(), "evil.example") {
		t.Errorf("the gateway's reasoning reached the client: %s", rec.Body.String())
	}
	if !strings.Contains(logged, "evil.example") {
		t.Errorf("the operator did not get the refused target: %q", logged)
	}
}

// ---- unary error reports: classified, and on the request's trace ----

func tracedContext(t *testing.T) (context.Context, *tracetest.SpanRecorder, func()) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "request")
	return ctx, sr, func() { span.End() }
}

// A NotFound or a validation failure is the caller's outcome: it goes to the
// refusal channel and onto the request's span as an event, never as a span
// error. A server fault still goes down the exception channel — on the same
// span. Both used to be reported as errors with no request context at all.
func TestWriteGRPCErrorCtx_ReportsByCodeOnTheRequestsTrace(t *testing.T) {
	for _, tc := range []struct {
		code      codes.Code
		wantLog   string
		wantError bool
	}{
		{codes.NotFound, "refused:", false},
		{codes.InvalidArgument, "refused:", false},
		{codes.PermissionDenied, "refused:", false},
		{codes.Internal, "error:", true},
		{codes.Unavailable, "error:", true},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			ctx, sr, end := tracedContext(t)
			logged := captureLog(t, func() {
				restgw.WriteGRPCErrorCtx(ctx, httptest.NewRecorder(), grpcstatus.Error(tc.code, "Svc.Method: operator detail"))
			})
			end()
			if !strings.Contains(logged, tc.wantLog) || !strings.Contains(logged, "operator detail") {
				t.Errorf("logged %q, want a %q line with the operator detail", logged, tc.wantLog)
			}
			spans := sr.Ended()
			if len(spans) != 1 {
				t.Fatalf("spans = %d, want 1", len(spans))
			}
			s := spans[0]
			if gotError := s.Status().Code == otelcodes.Error; gotError != tc.wantError {
				t.Errorf("span error status = %v, want %v", gotError, tc.wantError)
			}
			if tc.wantError {
				if len(s.Events()) == 0 {
					t.Error("the fault was not recorded on the request's span")
				}
				return
			}
			found := false
			for _, ev := range s.Events() {
				if ev.Name == "observx.refusal" {
					found = true
				}
			}
			if !found {
				t.Errorf("the refusal was not recorded on the request's span: %+v", s.Events())
			}
		})
	}
	// A non-status error is a fault.
	logged := captureLog(t, func() {
		restgw.WriteGRPCErrorCtx(context.Background(), httptest.NewRecorder(), errors.New("dial tcp 10.0.0.1:9090: refused"))
	})
	if !strings.Contains(logged, "error:") {
		t.Errorf("a non-status error must be reported as a fault: %q", logged)
	}
}

// ---- caches: Vary on every response that depends on a request header ----

func varyTokens(h http.Header) map[string]int {
	out := map[string]int{}
	for _, v := range h.Values("Vary") {
		for _, p := range strings.Split(v, ",") {
			out[strings.TrimSpace(p)]++
		}
	}
	return out
}

// A negotiated response says it depends on Accept, whichever format won, so a
// shared cache never hands one client's protobuf to another's JSON request.
func TestWriteResponse_NegotiatedResponseVariesOnAccept(t *testing.T) {
	msg, _ := structpb.NewStruct(map[string]any{"a": "b"})
	for _, accept := range []string{"application/json", "application/x-protobuf", ""} {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		restgw.WriteResponse(rec, http.StatusOK, msg, r)
		if got := varyTokens(rec.Header())["Accept"]; got != 1 {
			t.Errorf("Accept %q: Vary = %v, want Accept exactly once", accept, rec.Header().Values("Vary"))
		}
	}
	// Composes with what is already there and never repeats itself.
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	rec.Header().Set("Vary", "Origin")
	restgw.WriteResponseFiltered(rec, http.StatusOK, msg, nil, r)
	if v := varyTokens(rec.Header()); v["Origin"] != 1 || v["Accept"] != 1 {
		t.Errorf("Vary = %v, want Origin and Accept once each", rec.Header().Values("Vary"))
	}
	// Not negotiated (no request): always JSON, nothing to vary on.
	rec = httptest.NewRecorder()
	restgw.WriteResponse(rec, http.StatusOK, msg)
	if v := rec.Header().Values("Vary"); len(v) != 0 {
		t.Errorf("un-negotiated response carries Vary %v", v)
	}
}

// Every response the CORS middleware sees depends on Origin, including the
// ones that carry no CORS headers.
func TestCORSMiddleware_EveryResponseVariesOnOrigin(t *testing.T) {
	h := restgw.CORSMiddleware(restgw.CORSConfig{AllowedOrigins: []string{"https://app.example"}},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, tc := range []struct {
		name, origin, method string
		preflight            bool
	}{
		{"no origin", "", http.MethodGet, false},
		{"disallowed origin", "https://evil.example", http.MethodGet, false},
		{"allowed origin", "https://app.example", http.MethodGet, false},
		{"preflight allowed", "https://app.example", http.MethodOptions, true},
		{"preflight disallowed", "https://evil.example", http.MethodOptions, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/x", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.preflight {
				r.Header.Set("Access-Control-Request-Method", "POST")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if got := varyTokens(rec.Header())["Origin"]; got != 1 {
				t.Errorf("Vary = %v, want Origin exactly once", rec.Header().Values("Vary"))
			}
		})
	}
}

// ---- request id: the backend gets the id the gateway logged ----

// The id forwarded to the backend is the one the gateway validated, echoed
// and correlated — not the raw client header (which may be a log-forging
// payload), and not nothing when the gateway generated one.
func TestObservabilityMiddleware_BackendReceivesTheValidatedID(t *testing.T) {
	for _, tc := range []struct {
		name string
		mw   func(http.Handler) http.Handler
	}{
		{"observability", func(next http.Handler) http.Handler {
			return restgw.ObservabilityMiddleware(next, "svc", restgw.RequestIDConfig{})
		}},
		{"request id", func(next http.Handler) http.Handler {
			return restgw.RequestIDMiddleware(restgw.RequestIDConfig{}, next)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var forwarded []string
			backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				md, _ := metadata.FromOutgoingContext(r.Context())
				forwarded = md.Get("x-request-id")
			})
			h := tc.mw(restgw.MetadataPropagationMiddleware([]string{"X-Request-Id"}, backend))

			for _, sent := range []string{
				"",                       // none: generated
				"evil\r\nX-Forged: 1",    // malformed: replaced
				strings.Repeat("a", 200), // too long: replaced
				"client-id-123",          // valid: kept
			} {
				r := httptest.NewRequest(http.MethodGet, "/x", nil)
				if sent != "" {
					r.Header["X-Request-Id"] = []string{sent}
				}
				before := r.Header.Clone()
				forwarded = nil
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				echoed := rec.Header().Get("X-Request-Id")
				if len(forwarded) != 1 || forwarded[0] != echoed {
					t.Errorf("sent %q: backend got %q, gateway echoed %q — they must be the same id", sent, forwarded, echoed)
				}
				if sent == "client-id-123" && echoed != sent {
					t.Errorf("a valid client id was replaced: %q", echoed)
				}
				if fmt.Sprint(r.Header) != fmt.Sprint(before) {
					t.Errorf("sent %q: the caller's header map was modified", sent)
				}
			}
		})
	}
}

// ---- pagination links trust nothing the caller wrote ----

// The Link is origin-relative: a client resolves it against the URL it
// requested. Host and X-Forwarded-Proto are the caller's to write, so a link
// built from them advertised whatever origin the caller named.
func TestSetNextPageLink_IgnoresCallerSuppliedOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1/tasks?limit=10&cursor=old", nil)
	r.Host = "attacker.example"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "attacker.example")
	rec := httptest.NewRecorder()
	restgw.SetNextPageLink(rec, r, "next1")
	restgw.SetPrevPageLink(rec, r, "prev1")

	link := rec.Header().Get("Link")
	want := `</v1/tasks?cursor=next1&limit=10>; rel="next", </v1/tasks?cursor=prev1&limit=10>; rel="prev"`
	if link != want {
		t.Errorf("Link\n got:  %q\n want: %q", link, want)
	}
}

// A path that would read as a host, or that a client would normalise into
// one, stays on this origin.
func TestSetNextPageLink_PathNeverReadsAsAHost(t *testing.T) {
	for path, want := range map[string]string{
		"//evil.example/x": `</.//evil.example/x?cursor=c>; rel="next"`,
		`/\evil.example/x`: `</%5Cevil.example/x?cursor=c>; rel="next"`,
		"":                 `</?cursor=c>; rel="next"`,
	} {
		r := httptest.NewRequest(http.MethodGet, "/placeholder", nil)
		r.URL.Path, r.URL.RawPath = path, ""
		rec := httptest.NewRecorder()
		restgw.SetNextPageLink(rec, r, "c")
		if got := rec.Header().Get("Link"); got != want {
			t.Errorf("path %q: Link = %q, want %q", path, got, want)
		}
	}
}
