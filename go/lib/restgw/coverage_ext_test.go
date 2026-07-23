package restgw_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// ---- shared fakes ----------------------------------------------

// nonFlushWriter is an http.ResponseWriter that deliberately does
// NOT implement http.Flusher — exercises the SSE/event helpers'
// "writer can't flush" rejection arm.
type nonFlushWriter struct {
	header http.Header
	code   int
	body   strings.Builder
}

func newNonFlushWriter() *nonFlushWriter       { return &nonFlushWriter{header: http.Header{}} }
func (w *nonFlushWriter) Header() http.Header  { return w.header }
func (w *nonFlushWriter) WriteHeader(code int) { w.code = code }
func (w *nonFlushWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

// failFlushWriter implements http.Flusher but every Write fails —
// exercises the mid-stream client-disconnect arms of the SSE loop.
type failFlushWriter struct {
	header http.Header
	code   int
}

func newFailFlushWriter() *failFlushWriter           { return &failFlushWriter{header: http.Header{}} }
func (w *failFlushWriter) Header() http.Header       { return w.header }
func (w *failFlushWriter) WriteHeader(code int)      { w.code = code }
func (w *failFlushWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (w *failFlushWriter) Flush()                    {}

// errStore is a TicketStore whose Issue always fails — drives the
// issuer's 500 arm.
type errStore struct{}

func (errStore) Issue(context.Context, map[string]string, time.Duration) (string, error) {
	return "", errors.New("backend down")
}
func (errStore) Redeem(context.Context, string) (map[string]string, error) {
	return nil, restgw.ErrTicketInvalid
}

// fakeDriver lets each kvfs.Driver method be steered to an error to
// drive the download/upload failure arms.
type fakeDriver struct {
	statErr error
	statOK  kvfs.Info
	openErr error
	openRC  io.ReadSeekCloser
	putErr  error
}

func (d fakeDriver) PutFromTempFile(context.Context, string, string) (string, error) {
	return "", d.putErr
}
func (d fakeDriver) Open(context.Context, string) (io.ReadCloser, error) { return nil, d.openErr }
func (d fakeDriver) Delete(context.Context, string) error                { return nil }
func (d fakeDriver) Stat(context.Context, string) (kvfs.Info, error) {
	return d.statOK, d.statErr
}
func (d fakeDriver) OpenSeekable(context.Context, string) (io.ReadSeekCloser, error) {
	return d.openRC, d.openErr
}

// ---- auth.go ----------------------------------------------------

// ClassifyAuthScheme tolerates a nil request (defensive call site)
// and reports "no scheme".
func TestClassifyAuthScheme_NilRequest(t *testing.T) {
	if got := restgw.ClassifyAuthScheme(nil); got != restgw.AuthSchemeNone {
		t.Errorf("nil request = %q, want AuthSchemeNone", got)
	}
}

// WriteAuthError maps a gRPC status error to its canonical HTTP
// status (Unauthenticated -> 401); a plain error lands at 500.
func TestWriteAuthError_MapsStatusAndPlain(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteAuthError(rec, status.Error(codes.Unauthenticated, "nope"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status-error mapped to %d, want 401", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	restgw.WriteAuthError(rec2, errors.New("dial failed"))
	if rec2.Code != http.StatusInternalServerError {
		t.Errorf("plain error mapped to %d, want 500", rec2.Code)
	}
}

// SetUserMetadata copies + extends pre-existing outgoing metadata
// rather than dropping it (the md.Copy arm).
func TestSetUserMetadata_PreservesExistingOutgoing(t *testing.T) {
	seed := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-trace", "abc"))
	ctx := restgw.SetUserMetadata(seed, []byte{0x09})
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("outgoing metadata missing")
	}
	if got := md.Get("x-trace"); len(got) != 1 || got[0] != "abc" {
		t.Errorf("pre-existing outgoing metadata lost: %v", got)
	}
	if got := md.Get("x-w17-user"); len(got) != 1 {
		t.Errorf("principal metadata not stamped: %v", got)
	}
}

// GetUserMetadata treats an empty value the same as absent.
func TestGetUserMetadata_EmptyValueIsAbsent(t *testing.T) {
	in := metadata.New(map[string]string{"x-w17-user": ""})
	ctx := metadata.NewIncomingContext(context.Background(), in)
	if _, ok := restgw.GetUserMetadata(ctx); ok {
		t.Error("empty metadata value should report ok=false")
	}
}

// RequireAuth: a ticket sentinel failure becomes a clean 401
// (UNAUTHENTICATED), NOT a 500 — the ticket path maps its own
// status.
func TestRequireAuth_TicketSentinel401(t *testing.T) {
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		return nil, restgw.ErrWSAuthNoTicket
	})
	called := false
	h := restgw.RequireAuth(authFn, func(http.ResponseWriter, *http.Request) { called = true })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("ticket-sentinel failure = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UNAUTHENTICATED") {
		t.Errorf("body should carry UNAUTHENTICATED; got %s", rec.Body.String())
	}
	if called {
		t.Error("next should not run on auth failure")
	}
}

// RequireAuth: a non-ticket transport error falls through to
// WriteAuthError's 500/5xx treatment.
func TestRequireAuth_TransportError500(t *testing.T) {
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		return nil, errors.New("store unreachable")
	})
	h := restgw.RequireAuth(authFn, func(http.ResponseWriter, *http.Request) {})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("transport error = %d, want 500", rec.Code)
	}
}

// RequireAuth: on success it stashes the principal bytes so a
// downstream streaming handler recovers them.
func TestRequireAuth_StashesPrincipal(t *testing.T) {
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		return []byte("principal-bytes"), nil
	})
	var seen []byte
	h := restgw.RequireAuth(authFn, func(_ http.ResponseWriter, r *http.Request) {
		seen = restgw.AuthUserDataFromContext(r.Context())
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if string(seen) != "principal-bytes" {
		t.Errorf("principal not stashed for next handler; got %q", seen)
	}
}

// ---- negotiate.go ----------------------------------------------

// RequestFormat / ResponseFormat tolerate a nil request and an
// Accept header with no recognised media type (both default JSON).
func TestNegotiate_NilAndUnrecognised(t *testing.T) {
	if restgw.RequestFormat(nil) != restgw.WireJSON {
		t.Error("nil request should decode as JSON")
	}
	if restgw.ResponseFormat(nil) != restgw.WireJSON {
		t.Error("nil request should encode as JSON")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept", "text/html, image/png")
	if restgw.ResponseFormat(r) != restgw.WireJSON {
		t.Error("Accept with no recognised type should fall back to JSON")
	}
}

// ---- pagination.go ---------------------------------------------

// SetPrevPageLink is a no-op for an empty cursor.
func TestSetPrevPageLink_EmptyCursorNoOp(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.SetPrevPageLink(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "")
	if rec.Header().Get("Link") != "" {
		t.Errorf("empty cursor must not emit a Link header; got %q", rec.Header().Get("Link"))
	}
}

// nextPageURL derives the https scheme from a TLS connection when no
// X-Forwarded-Proto header is present.
func TestSetNextPageLink_TLSDerivesHTTPS(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/list?limit=10", nil)
	r.TLS = &tls.ConnectionState{}
	r.Host = "api.example.com"
	restgw.SetNextPageLink(rec, r, "cursor-2")
	link := rec.Header().Get("Link")
	if !strings.HasPrefix(link, "<https://api.example.com/list?") {
		t.Errorf("TLS request should produce an https Link; got %q", link)
	}
}

// ---- json.go ----------------------------------------------------

// errBody is a request body whose Read always fails — drives
// DecodeRequest's read-error arm.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("read blew up") }
func (errBody) Close() error             { return nil }

// DecodeRequest surfaces a read failure as an error.
func TestDecodeRequest_ReadError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", errBody{})
	err := restgw.DecodeRequest(r, &wrapperspb.StringValue{})
	if err == nil || !strings.Contains(err.Error(), "reading request body") {
		t.Errorf("want read error, got %v", err)
	}
}

// DecodeRequest in protobuf mode: an empty body is a no-op; a
// malformed body is a decode error.
func TestDecodeRequest_ProtobufEmptyAndBad(t *testing.T) {
	empty := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	empty.Header.Set("Content-Type", "application/protobuf")
	if err := restgw.DecodeRequest(empty, &wrapperspb.StringValue{}); err != nil {
		t.Errorf("empty protobuf body should be a no-op; got %v", err)
	}

	bad := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("\xff\xff\xffnot-proto"))
	bad.Header.Set("Content-Type", "application/protobuf")
	if err := restgw.DecodeRequest(bad, &wrapperspb.StringValue{}); err == nil {
		t.Error("malformed protobuf body should error")
	}
}

// MarshalProto surfaces a protojson marshal failure (invalid UTF-8
// in a string field).
func TestMarshalProto_InvalidUTF8Errors(t *testing.T) {
	if _, err := restgw.MarshalProto(wrapperspb.String("\xff\xfe")); err == nil {
		t.Error("invalid UTF-8 string should fail to marshal")
	}
}

// WriteResponse with no trailing request defaults to JSON.
func TestWriteResponse_NoRequestDefaultsJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteResponse(rec, http.StatusOK, wrapperspb.String("hi"))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// WriteResponse negotiates a binary protobuf body when the request
// Accepts it.
func TestWriteResponse_NegotiatesProtobuf(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept", "application/protobuf")
	restgw.WriteResponse(rec, http.StatusOK, wrapperspb.String("hi"), r)
	if ct := rec.Header().Get("Content-Type"); ct != "application/protobuf" {
		t.Errorf("Content-Type = %q, want application/protobuf", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("expected a non-empty protobuf body")
	}
}

// WriteResponse degrades a marshal failure to a JSON 500 envelope
// (errors are never negotiated).
func TestWriteResponse_MarshalErrorIs500(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteResponse(rec, http.StatusOK, wrapperspb.String("\xff"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("marshal failure = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Errorf("body should be the JSON error envelope; got %s", rec.Body.String())
	}
}

// ---- ticket.go --------------------------------------------------

// MountTicketIssuer wires a POST issuer on a chi router + returns a
// usable store; a POST mints a redeemable ticket.
func TestMountTicketIssuer_MintsRedeemable(t *testing.T) {
	r := chi.NewRouter()
	store := restgw.MountTicketIssuer(r, "/ws/ticket", func(*http.Request) (map[string]string, error) {
		return map[string]string{"authorization": "Bearer z"}, nil
	})
	defer store.Close()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ws/ticket", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("issuer POST = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ticket") {
		t.Errorf("response should carry a ticket; got %s", rec.Body.String())
	}
}

// Issue with a non-positive TTL falls back to the 30s default
// (the ticket is still redeemable immediately).
func TestTicketStore_IssueDefaultTTL(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	defer store.Close()
	ticket, err := store.Issue(context.Background(), map[string]string{"k": "v"}, 0)
	if err != nil || ticket == "" {
		t.Fatalf("Issue with zero TTL: %q, %v", ticket, err)
	}
	if _, err := store.Redeem(context.Background(), ticket); err != nil {
		t.Errorf("default-TTL ticket should redeem immediately: %v", err)
	}
}

// NewTicketIssuer panics on a missing Principal or Store — config
// errors surface at construction, not on first serve.
func TestNewTicketIssuer_PanicsOnMissingConfig(t *testing.T) {
	assertPanics(t, "nil Principal", func() {
		restgw.NewTicketIssuer(restgw.TicketIssuerConfig{Store: restgw.NewMemoryTicketStore()})
	})
	assertPanics(t, "nil Store", func() {
		restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
			Principal: func(*http.Request) (map[string]string, error) { return nil, nil },
		})
	})
}

// A Store.Issue failure surfaces as a 500 from the issuer endpoint.
func TestTicketIssuer_StoreFailure500(t *testing.T) {
	issuer := restgw.NewTicketIssuer(restgw.TicketIssuerConfig{
		Principal: func(*http.Request) (map[string]string, error) { return map[string]string{}, nil },
		Store:     errStore{},
	})
	rec := httptest.NewRecorder()
	issuer.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ticket", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

// ---- upload.go --------------------------------------------------

// TestWriteUploadError_MasksInternalError — Q36-restgw-1. A non-sentinel
// (internal) error wraps driver / tmp-file failures that carry server
// topology (S3 endpoint / creds, absolute FS paths); the response body
// must NOT reflect it — only a generic message, like WriteGRPCError.
func TestWriteUploadError_MasksInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteUploadError(rec, errors.New("restgw: driver put: s3://secret-bucket dial 10.0.0.5:9000: connection refused"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"secret-bucket", "10.0.0.5", "driver put", "connection refused"} {
		if strings.Contains(body, leak) {
			t.Errorf("response body leaks internal detail %q:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "internal error") {
		t.Errorf("body should carry the generic message; got %s", body)
	}
}

// WriteUploadError maps each upload sentinel to its canonical HTTP
// status; an unknown error lands at 500.
func TestWriteUploadError_StatusTable(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{restgw.ErrFilePartMissing, http.StatusBadRequest},
		{restgw.ErrUploadTooLarge, http.StatusRequestEntityTooLarge},
		{restgw.ErrUploadBadExt, http.StatusBadRequest},
		{errors.New("disk gone"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		restgw.WriteUploadError(rec, c.err)
		if rec.Code != c.want {
			t.Errorf("WriteUploadError(%v) = %d, want %d", c.err, rec.Code, c.want)
		}
	}
}

// ProcessFilePart rejects a nil driver and an empty form name before
// touching the request body.
func TestProcessFilePart_ConfigGuards(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/u", nil)
	if _, err := restgw.ProcessFilePart(context.Background(), r, restgw.FilePartConfig{FormName: "f"}); err == nil {
		t.Error("nil driver should be rejected")
	}
	d := fakeDriver{}
	if _, err := restgw.ProcessFilePart(context.Background(), r, restgw.FilePartConfig{Driver: d}); err == nil {
		t.Error("empty form name should be rejected")
	}
}

// ProcessFilePart surfaces a driver Put failure (after streaming the
// body to the temp file).
func TestProcessFilePart_DriverPutError(t *testing.T) {
	body, ct := buildMultipart(t, map[string]string{"file": "ok.pdf=payload"})
	r := httptest.NewRequest(http.MethodPost, "/u", body)
	r.Header.Set("Content-Type", ct)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	_, err := restgw.ProcessFilePart(context.Background(), r, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            fakeDriver{putErr: errors.New("backend write failed")},
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	})
	if err == nil || !strings.Contains(err.Error(), "driver put") {
		t.Errorf("want driver-put error, got %v", err)
	}
}

// ProcessFilePart surfaces a temp-dir creation failure (TmpDir under
// a non-directory path).
func TestProcessFilePart_TmpDirError(t *testing.T) {
	body, ct := buildMultipart(t, map[string]string{"file": "ok.pdf=payload"})
	r := httptest.NewRequest(http.MethodPost, "/u", body)
	r.Header.Set("Content-Type", ct)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	_, err := restgw.ProcessFilePart(context.Background(), r, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            fakeDriver{},
		TmpDir:            "/dev/null/cannot-mkdir-under-a-file",
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	})
	if err == nil || !strings.Contains(err.Error(), "mkdir tmp") {
		t.Errorf("want mkdir-tmp error, got %v", err)
	}
}

// ProcessFilePart rejects a file with no extension when the
// allowlist is non-wildcard.
func TestProcessFilePart_NoExtensionRejected(t *testing.T) {
	body, ct := buildMultipart(t, map[string]string{"file": "noextension=payload"})
	r := httptest.NewRequest(http.MethodPost, "/u", body)
	r.Header.Set("Content-Type", ct)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	_, err := restgw.ProcessFilePart(context.Background(), r, restgw.FilePartConfig{
		FormName:          "file",
		Driver:            fakeDriver{},
		MaxSizeBytes:      1024,
		AllowedExtensions: []string{"pdf"},
	})
	if !errors.Is(err, restgw.ErrUploadBadExt) {
		t.Errorf("file without extension should be rejected; got %v", err)
	}
}

// ---- download.go ------------------------------------------------

// ServeDownload returns 404 when the URL is exactly the prefix plus
// a trailing slash (no object remainder).
func TestServeDownload_EmptyRemainder404(t *testing.T) {
	cfg := restgw.NewDownloadConfig("/files",
		[]restgw.DownloadField{{BucketPath: "/a", Driver: fakeDriver{}}},
		"", false, false, false)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/files/", nil), cfg)
	if rec.Code != http.StatusNotFound {
		t.Errorf("trailing-slash-only path = %d, want 404", rec.Code)
	}
}

// ServeDownload maps a non-NotFound Stat failure to 500.
func TestServeDownload_StatError500(t *testing.T) {
	cfg := restgw.NewDownloadConfig("/files",
		[]restgw.DownloadField{{BucketPath: "/a", Driver: fakeDriver{statErr: errors.New("io error")}}},
		"", false, false, false)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/files/a/key", nil), cfg)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("stat io error = %d, want 500", rec.Code)
	}
}

// ServeDownload maps an OpenSeekable NotFound to 404 and a generic
// OpenSeekable failure to 500 (Stat already succeeded).
func TestServeDownload_OpenErrors(t *testing.T) {
	notFound := restgw.NewDownloadConfig("/files",
		[]restgw.DownloadField{{BucketPath: "/a", Driver: fakeDriver{openErr: kvfs.ErrNotFound}}},
		"", false, false, false)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/files/a/key", nil), notFound)
	if rec.Code != http.StatusNotFound {
		t.Errorf("open NotFound = %d, want 404", rec.Code)
	}

	ioErr := restgw.NewDownloadConfig("/files",
		[]restgw.DownloadField{{BucketPath: "/a", Driver: fakeDriver{openErr: errors.New("disk gone")}}},
		"", false, false, false)
	rec2 := httptest.NewRecorder()
	restgw.ServeDownload(rec2, httptest.NewRequest(http.MethodGet, "/files/a/key", nil), ioErr)
	if rec2.Code != http.StatusInternalServerError {
		t.Errorf("open io error = %d, want 500", rec2.Code)
	}
}

// matchBucket skips a field with an empty bucket path (defensive
// guard) and falls through to 404.
func TestServeDownload_EmptyBucketPathSkipped(t *testing.T) {
	cfg := restgw.NewDownloadConfig("/files",
		[]restgw.DownloadField{{BucketPath: "/", Driver: fakeDriver{}}},
		"", false, false, false)
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/files/anything/key", nil), cfg)
	if rec.Code != http.StatusNotFound {
		t.Errorf("empty bucket path should be skipped -> 404; got %d", rec.Code)
	}
}

// ---- otel.go ----------------------------------------------------

// MountMetricsListener returns "" when the metrics port env is
// unset, and binds (then logs an error) when the chosen port is
// already occupied.
func TestMountMetricsListener_UnsetAndBindError(t *testing.T) {
	if addr := restgw.MountMetricsListener("X", func(string) string { return "" }, func(string, ...any) {}); addr != "" {
		t.Errorf("unset port should yield empty addr; got %q", addr)
	}

	// An out-of-range port makes the goroutine's ListenAndServe
	// fail deterministically, routing through the log arm (and
	// leaving nothing bound to clean up).
	const badPort = "999999"
	logged := make(chan string, 1)
	addr := restgw.MountMetricsListener("X",
		func(k string) string {
			if k == "X_METRICS_PORT" {
				return badPort
			}
			return ""
		},
		func(format string, args ...any) {
			select {
			case logged <- format:
			default:
			}
		})
	if addr != ":"+badPort {
		t.Errorf("addr = %q, want :%s", addr, badPort)
	}
	select {
	case <-logged:
	case <-time.After(2 * time.Second):
		t.Error("expected the metrics listener to log a bind failure")
	}
}

// ---- streaming.go: SSE -----------------------------------------

// WriteSSEHeaders rejects a ResponseWriter that can't flush.
func TestWriteSSEHeaders_NoFlusher(t *testing.T) {
	if _, ok := restgw.WriteSSEHeaders(newNonFlushWriter()); ok {
		t.Error("a non-flusher writer should report ok=false")
	}
}

// WriteSSEEvent surfaces a marshal failure and a mid-write client
// disconnect.
func TestWriteSSEEvent_MarshalAndWriteErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := restgw.WriteSSEEvent(rec, rec, wrapperspb.String("\xff")); err == nil {
		t.Error("invalid UTF-8 event should fail to marshal")
	}
	fw := newFailFlushWriter()
	if err := restgw.WriteSSEEvent(fw, fw, wrapperspb.String("ok")); err == nil {
		t.Error("a failing writer should surface the write error")
	}
}

// WriteSSEEventWithTimeout installs + clears a write deadline on a
// writer that supports SetWriteDeadline (a real TCP conn).
func TestWriteSSEEventWithTimeout_DeadlineInstalled(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := restgw.WriteSSEHeaders(w)
		if !ok {
			return
		}
		// Real http.ResponseWriter over TCP supports
		// SetWriteDeadline, so the deadline arm runs.
		err := restgw.WriteSSEEventWithTimeout(w, flusher, wrapperspb.String("hello"), 2*time.Second)
		if err != nil {
			got <- "err:" + err.Error()
			return
		}
		got <- "ok"
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
	select {
	case s := <-got:
		if s != "ok" {
			t.Errorf("deadline write reported %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Error("handler never completed")
	}
}

// WriteSSEGRPCError emits an INTERNAL event for a non-status error.
func TestWriteSSEGRPCError_NonStatusInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher, _ := restgw.WriteSSEHeaders(rec)
	restgw.WriteSSEGRPCError(rec, flusher, errors.New("raw failure"))
	if !strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Errorf("non-status error should map to INTERNAL; got %s", rec.Body.String())
	}
	// nil error is a no-op.
	rec2 := httptest.NewRecorder()
	f2, _ := restgw.WriteSSEHeaders(rec2)
	restgw.WriteSSEGRPCError(rec2, f2, nil)
	if strings.Contains(rec2.Body.String(), "event: error") {
		t.Error("nil error should not emit an error frame")
	}
}

// ---- streaming.go: WebSocket -----------------------------------

// wsRoundTrip stands up a one-shot WS server that runs `serverFn`
// against the accepted conn, then dials it and runs `clientFn`.
func wsRoundTrip(t *testing.T, dialOpts *websocket.DialOptions,
	serverFn func(ctx context.Context, conn *websocket.Conn),
	clientFn func(ctx context.Context, conn *websocket.Conn)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		serverFn(r.Context(), conn)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], dialOpts)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	clientFn(ctx, conn)
}

// WSFormat reports WireProto when the client negotiated the w17.pb
// subprotocol, WireJSON otherwise.
func TestWSFormat_ProtoSubprotocol(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.pb"}},
		func(_ context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			if got := restgw.WSFormat(conn); got != restgw.WireProto {
				t.Errorf("server WSFormat = %v, want WireProto", got)
			}
		},
		func(_ context.Context, conn *websocket.Conn) {
			if got := restgw.WSFormat(conn); got != restgw.WireProto {
				t.Errorf("client WSFormat = %v, want WireProto", got)
			}
		})
	// nil conn defaults to JSON.
	if restgw.WSFormat(nil) != restgw.WireJSON {
		t.Error("nil conn should default to WireJSON")
	}
}

// WSWriteProto / WSReadProto round-trip a binary protobuf frame.
func TestWSReadWriteProto_BinaryRoundTrip(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.pb"}},
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = restgw.WSWriteProto(ctx, conn, wrapperspb.String("binary-payload"), restgw.WireProto)
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			if err := restgw.WSReadProto(ctx, conn, &got, restgw.WireProto); err != nil {
				t.Fatalf("WSReadProto(proto): %v", err)
			}
			if got.Value != "binary-payload" {
				t.Errorf("got %q, want binary-payload", got.Value)
			}
		})
}

// WSReadProto rejects a text frame when a binary (proto) frame was
// expected.
func TestWSReadProto_ExpectedBinaryGotText(t *testing.T) {
	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = conn.Write(ctx, websocket.MessageText, []byte("{}"))
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			if err := restgw.WSReadProto(ctx, conn, &got, restgw.WireProto); err == nil {
				t.Error("text frame under proto format should error")
			}
		})
}

// WSReadProtoOrEOF decodes a normal data frame (eof=false) and then
// recognises the client-stream half-close marker as eof=true — the basis
// of the WS client-stream protocol (the client signals "done sending"
// without closing the socket so the server can still reply).
func TestWSReadProtoOrEOF_DataThenMarker(t *testing.T) {
	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			eof, err := restgw.WSReadProtoOrEOF(ctx, conn, &got)
			if err != nil || eof {
				t.Errorf("first frame: eof=%v err=%v, want a data frame", eof, err)
			}
			if got.Value != "hi" {
				t.Errorf("data frame decoded %q, want hi", got.Value)
			}
			eof2, err2 := restgw.WSReadProtoOrEOF(ctx, conn, &got)
			if err2 != nil {
				t.Errorf("marker frame read error: %v", err2)
			}
			if !eof2 {
				t.Error("the WSClientStreamEOF marker frame should report eof=true")
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			_ = restgw.WSWriteProto(ctx, conn, wrapperspb.String("hi"))
			_ = conn.Write(ctx, websocket.MessageText, []byte(restgw.WSClientStreamEOF))
		})
}

// WSReadProto rejects a binary frame carrying invalid protobuf.
func TestWSReadProto_InvalidProtobuf(t *testing.T) {
	wsRoundTrip(t, &websocket.DialOptions{Subprotocols: []string{"w17.pb"}},
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = conn.Write(ctx, websocket.MessageBinary, []byte{0xff, 0xff, 0xff})
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			if err := restgw.WSReadProto(ctx, conn, &got, restgw.WireProto); err == nil {
				t.Error("invalid protobuf bytes should error")
			}
		})
}

// WSReadProto rejects a binary frame when a text (JSON) frame was
// expected.
func TestWSReadProto_ExpectedTextGotBinary(t *testing.T) {
	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			_ = conn.Write(ctx, websocket.MessageBinary, []byte{0x01})
		},
		func(ctx context.Context, conn *websocket.Conn) {
			var got wrapperspb.StringValue
			// No format arg -> defaults to JSON (covers wsFormatOpt).
			if err := restgw.WSReadProto(ctx, conn, &got); err == nil {
				t.Error("binary frame under JSON format should error")
			}
		})
}

// WSWriteProto surfaces a JSON marshal failure (invalid UTF-8).
func TestWSWriteProto_JSONMarshalError(t *testing.T) {
	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			if err := restgw.WSWriteProto(ctx, conn, wrapperspb.String("\xff"), restgw.WireJSON); err == nil {
				t.Error("invalid UTF-8 should fail the JSON marshal")
			}
		},
		func(context.Context, *websocket.Conn) {})
}

// WSWriteError / WSWriteErrorWithDetails write the canonical error
// envelope as a text frame, then close the conn with a policy
// violation.
func TestWSWriteError_EnvelopeThenClose(t *testing.T) {
	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			restgw.WSWriteError(ctx, conn, "INVALID_ARGUMENT", "bad frame")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read envelope: %v", err)
			}
			if typ != websocket.MessageText || !strings.Contains(string(data), "INVALID_ARGUMENT") {
				t.Errorf("unexpected envelope frame: typ=%v data=%s", typ, data)
			}
			// Subsequent read sees the close.
			if _, _, err := conn.Read(ctx); err == nil {
				t.Error("expected the conn to be closed after the error envelope")
			}
		})
}

// WSWriteGRPCError maps a status error to its code; a non-status
// error to INTERNAL; nil is a no-op.
func TestWSWriteGRPCError_StatusNonStatusNil(t *testing.T) {
	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			restgw.WSWriteGRPCError(ctx, conn, status.Error(codes.NotFound, "missing"))
		},
		func(ctx context.Context, conn *websocket.Conn) {
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			// Canonical UPPER_SNAKE name, not grpc's PascalCase.
			if !strings.Contains(string(data), "NOT_FOUND") {
				t.Errorf("status code should ride the envelope; got %s", data)
			}
		})

	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			restgw.WSWriteGRPCError(ctx, conn, errors.New("raw"))
		},
		func(ctx context.Context, conn *websocket.Conn) {
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !strings.Contains(string(data), "INTERNAL") {
				t.Errorf("non-status error should map to INTERNAL; got %s", data)
			}
		})

	// nil error: no frame, conn stays usable until normal close.
	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			restgw.WSWriteGRPCError(ctx, conn, nil)
			_ = conn.Close(websocket.StatusNormalClosure, "")
		},
		func(context.Context, *websocket.Conn) {})
}

// ---- recover.go: WS pump panic recovery ------------------------

// RecoverWSPump converts a panic in a spawned WS pump goroutine into
// a clean internal-error close (the panic value never reaches the
// client); the no-panic path is a no-op, and a nil ctx falls back to
// Background.
func TestRecoverWSPump_RecoversPanicAndCloses(t *testing.T) {
	// observx routes the panic to a log fallback — silence it.
	orig := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(orig)

	wsRoundTrip(t, nil,
		func(_ context.Context, conn *websocket.Conn) {
			// No-panic arm: deferred recover with nothing in flight.
			func() { defer restgw.RecoverWSPump(context.Background(), conn, "noop") }()
			// Panic arm with a nil ctx (Background fallback).
			defer restgw.RecoverWSPump(context.TODO(), conn, "Svc.Pump")
			panic("pump exploded: internal detail")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			if _, _, err := conn.Read(ctx); err == nil {
				t.Error("expected the conn to be closed after a pump panic")
			}
		})
}

// ---- w17events.go: handler I/O failure arms --------------------

// HandleW17Events returns silently when the writer can't flush
// (subscribe already succeeded; the SSE headers can't go out).
func TestHandleW17Events_NoFlusherReturns(t *testing.T) {
	source := restgw.NewStaticEventSource(restgw.Event{Topic: "X", Data: []byte("{}")})
	req := httptest.NewRequest(http.MethodGet, "/w17-events?topics=X", nil)
	w := newNonFlushWriter()
	restgw.HandleW17Events(w, req, source, time.Hour)
	// No event frame should have been written (we bailed at the
	// flush check), and the body stays empty.
	if strings.Contains(w.body.String(), "event: X") {
		t.Errorf("no frame should be written without a flusher; got %q", w.body.String())
	}
}

// HandleW17Events applies the 30s heartbeat default when given a
// non-positive interval, then unwinds on ctx cancel.
func TestHandleW17Events_HeartbeatDefaultThenCancel(t *testing.T) {
	source := &blockingSource{}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/w17-events?topics=X", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(rec, req, source, 0) // 0 -> default 30s
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not unwind on ctx cancel")
	}
}

// HandleW17Events returns when a heartbeat write fails (client
// disconnect during the keep-alive tick).
func TestHandleW17Events_HeartbeatWriteErrorReturns(t *testing.T) {
	source := &blockingSource{}
	req := httptest.NewRequest(http.MethodGet, "/w17-events?topics=X", nil)
	w := newFailFlushWriter()
	done := make(chan struct{})
	go func() {
		// Short heartbeat -> the first tick's write fails fast.
		restgw.HandleW17Events(w, req, source, 10*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler should return after a heartbeat write failure")
	}
}

// HandleW17Events returns when an event-frame write fails.
func TestHandleW17Events_FrameWriteErrorReturns(t *testing.T) {
	source := restgw.NewStaticEventSource(restgw.Event{Topic: "X", Data: []byte(`{"a":1}`)})
	req := httptest.NewRequest(http.MethodGet, "/w17-events?topics=X", nil)
	w := newFailFlushWriter()
	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(w, req, source, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler should return after a frame write failure")
	}
}

// ---- w17events_eventbus.go: ensureOpen failure arms ------------

// errFactory's Subscriber always fails — drives ensureOpen's
// factory-error arm (and, via Subscribe, the error propagation in
// EventbusEventSource.Subscribe).
type errFactory struct{}

func (errFactory) Subscriber(context.Context, string) (eventbus.Subscriber, error) {
	return nil, errors.New("no broker")
}

// subErrFactory hands back a subscriber whose Subscribe fails.
type subErrFactory struct{}

func (subErrFactory) Subscriber(context.Context, string) (eventbus.Subscriber, error) {
	return badSubscriber{}, nil
}

type badSubscriber struct{}

func (badSubscriber) Subscribe(context.Context, string, eventbus.HandlerFunc) error {
	return errors.New("subscribe refused")
}
func (badSubscriber) Drain(context.Context) error { return nil }

func TestEventbusSource_EnsureOpenErrors(t *testing.T) {
	t.Run("subscriber factory error", func(t *testing.T) {
		src := restgw.NewEventbusEventSource(errFactory{}, []string{"default"}, rawTranscode, nil)
		if _, err := src.Subscribe(context.Background(), []string{"x"}, nil); err == nil ||
			!strings.Contains(err.Error(), "subscriber for channel") {
			t.Errorf("want subscriber-for-channel error, got %v", err)
		}
	})
	t.Run("subscribe error", func(t *testing.T) {
		src := restgw.NewEventbusEventSource(subErrFactory{}, []string{"default"}, rawTranscode, nil)
		if _, err := src.Subscribe(context.Background(), []string{"x"}, nil); err == nil ||
			!strings.Contains(err.Error(), "subscribe") {
			t.Errorf("want subscribe error, got %v", err)
		}
	})
}

// ---- helpers ----------------------------------------------------

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected a panic", what)
		}
	}()
	fn()
}
