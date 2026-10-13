package restgw_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// A client must not be able to put a value in a key the gateway reserves —
// the principal, its data scopes and labels, the transaction keys,
// grpc-gateway's forwarded-header namespace — on any route the REST gateway
// serves, whether the route authenticates or not. These tests assemble the
// middleware chain in the order the generated main does and drive one route
// per surface (unary without auth, unary with auth, SSE, WebSocket, the event
// hub, upload, download) against a real gRPC backend that records the
// metadata it receives.
//
// The chain is configured the way only a misconfiguration would configure it:
// every reserved key named in the propagation list, as a rename target and as
// a default stamp. The generator refuses such a list; the runtime must refuse
// it too, because a gateway that does not come from the generator — or one a
// later edit routes around it — otherwise forwards the client's value.

// forgedHeaders is what the client sends on every request: one header per
// reserved key, in the spellings a client can reach it by, plus the benign
// headers the decoy assertions expect to arrive.
func forgedHeaders() http.Header {
	h := http.Header{}
	h.Set("X-W17-User", base64.StdEncoding.EncodeToString([]byte("forged-principal")))
	h.Set("X-W17-Scope-Tenant_id", "forged-tenant")
	h.Set("W17-Label-Tenant", "forged-label")
	h.Set("W17-Tx-Id", "forged-tx")
	h.Set("W17-Conn-Id", "forged-conn")
	h.Set("Grpcgateway-Authorization", "forged")
	h.Set("Grpc-Metadata-X-W17-User", "forged")
	h.Set("X-W17-Credential", "forged-capability")
	h.Set("X-W17-Device", "forged-device")
	h.Set("Grpc-Metadata-X-W17-Device", "forged-device")
	// Benign headers that rename rules route INTO reserved keys.
	h.Set("X-Who", "forged-principal")
	h.Set("X-Org", "forged-org")
	// Decoys: an ordinary propagated header and an ordinary rename must
	// still arrive, or "nothing reserved arrived" would hold for a chain
	// that forwards nothing at all.
	h.Set("X-Tenant-Hint", "hint")
	h.Set("Accept-Language", "cs")
	return h
}

// reservedKeys are the metadata keys no client value may reach.
var reservedKeys = []string{
	"x-w17-user", "x-w17-scope-tenant_id", "x-w17-scope-org_id", "w17-label-tenant",
	"w17-tx-id", "w17-conn-id", "grpcgateway-authorization",
	"grpc-metadata-x-w17-user", "x-w17-credential", "x-w17-device", "grpc-metadata-x-w17-device",
}

// edgeChain wraps router exactly as the generated main does, with every
// reserved key configured into the metadata middlewares.
func edgeChain(router http.Handler) http.Handler {
	stamped := restgw.MetadataDefaultStampMiddleware([]restgw.DefaultMetadataStamp{
		{Key: "x-w17-scope-org_id", Value: "stamped-org"},
		{Key: "x-w17-user", Value: "stamped-principal"},
		{Key: "w17-format-locale", Value: "cs-CZ"},
	}, router)
	renamed := restgw.MetadataRenameMiddleware([]restgw.HeaderRenameRule{
		{HTTP: "Accept-Language", Metadata: "w17-language"},
		{HTTP: "X-Who", Metadata: "x-w17-user"},
		{HTTP: "X-Org", Metadata: "X-W17-Scope-Org_id"},
		{HTTP: "X-Org", Metadata: "w17-tx-id"},
	}, stamped)
	propagated := restgw.MetadataPropagationMiddleware([]string{
		"X-Request-Id", "X-Tenant-Hint",
		"X-W17-User", "X-W17-Scope-Tenant_id", "W17-Label-Tenant",
		"W17-Tx-Id", "W17-Conn-Id", "Grpcgateway-Authorization",
		"Grpc-Metadata-X-W17-User", "X-W17-Credential", "X-W17-Device", "Grpc-Metadata-X-W17-Device",
	}, renamed)
	observed := restgw.ObservabilityMiddleware(propagated, "edge-test", restgw.RequestIDConfig{})
	return restgw.RecoverPanicMiddleware(restgw.StripReservedHeadersMiddleware(observed))
}

// recordingBackend is a gRPC health server that records the incoming
// metadata of every call it serves.
type recordingBackend struct {
	mu   sync.Mutex
	seen []metadata.MD
	conn *grpc.ClientConn
}

func (b *recordingBackend) record(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	b.mu.Lock()
	b.seen = append(b.seen, md.Copy())
	b.mu.Unlock()
}

// take returns the metadata of the one call the surface made. The HTTP
// handler can hand its headers over before its backend call is recorded (a
// streaming surface dials after it starts the response), so it waits for
// the call rather than reading whatever has arrived.
func (b *recordingBackend) take(t *testing.T) metadata.MD {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		n := len(b.seen)
		b.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.seen) != 1 {
		t.Fatalf("backend saw %d calls, want 1", len(b.seen))
	}
	md := b.seen[0]
	b.seen = nil
	return md
}

func startRecordingBackend(t *testing.T) *recordingBackend {
	t.Helper()
	b := &recordingBackend{}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			b.record(ctx)
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			b.record(ss.Context())
			return h(srv, ss)
		}),
	)
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, hs)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	b.conn = conn
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return b
}

func (b *recordingBackend) unary(ctx context.Context) error {
	_, err := grpc_health_v1.NewHealthClient(b.conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	return err
}

func (b *recordingBackend) stream(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := grpc_health_v1.NewHealthClient(b.conn).Watch(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return err
	}
	_, err = st.Recv()
	return err
}

// gatewayPrincipal is what the authenticated route's auth stamps.
var gatewayPrincipal = []byte("gateway-verified-principal")

// edgeRouter mounts one route per surface, each calling the backend the way
// its generated handler does: with the request's own context, after its own
// stamping where it has any.
func edgeRouter(t *testing.T, b *recordingBackend, handlerHeaders chan<- http.Header) http.Handler {
	t.Helper()
	fail := func(w http.ResponseWriter, err error) {
		t.Errorf("backend call: %v", err)
		w.WriteHeader(http.StatusBadGateway)
	}
	// seenBy hands the headers the handler saw to the test once the
	// handler has returned, which is also when its backend call is done.
	seenBy := func(r *http.Request) func() {
		h := r.Header.Clone()
		return func() { handlerHeaders <- h }
	}
	authed := func(_ context.Context, _ *http.Request) ([]byte, error) { return gatewayPrincipal, nil }

	uploads := memory.New()
	downloads := memory.New()
	tmp := t.TempDir()
	if err := writeMemoryObject(t, downloads, "files/a.txt"); err != nil {
		t.Fatal(err)
	}
	dl := restgw.NewDownloadConfig("/download", []restgw.DownloadField{{BucketPath: "files", Driver: downloads}}, "", false, false, false)

	r := chi.NewRouter()
	r.Get("/public", func(w http.ResponseWriter, r *http.Request) {
		defer seenBy(r)()
		if err := b.unary(r.Context()); err != nil {
			fail(w, err)
		}
	})
	r.Get("/authed", func(w http.ResponseWriter, r *http.Request) {
		defer seenBy(r)()
		data, err := authed(r.Context(), r)
		if err != nil {
			restgw.WriteAuthError(w, err)
			return
		}
		ctx := restgw.SetUserMetadata(r.Context(), data)
		ctx = metadata.AppendToOutgoingContext(ctx, restgw.ScopeKey("tenant_id"), "verified-tenant")
		if err := b.unary(ctx); err != nil {
			fail(w, err)
		}
	})
	r.Get("/sse", func(w http.ResponseWriter, r *http.Request) {
		defer seenBy(r)()
		flusher, ok := restgw.WriteSSEHeaders(w)
		if !ok {
			t.Error("no flusher")
			return
		}
		if err := b.stream(r.Context()); err != nil {
			restgw.WriteSSEGRPCErrorCtx(r.Context(), w, flusher, err)
			return
		}
		_ = restgw.WriteSSEEvent(w, flusher, &grpc_health_v1.HealthCheckResponse{})
	})
	r.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		defer seenBy(r)()
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := b.stream(r.Context()); err != nil {
			t.Errorf("backend stream: %v", err)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	r.Get("/w17-events", restgw.RequireAuth(restgw.NoAuth(), func(w http.ResponseWriter, r *http.Request) {
		defer seenBy(r)()
		if err := b.stream(r.Context()); err != nil {
			fail(w, err)
		}
	}))
	r.Post("/upload", func(w http.ResponseWriter, r *http.Request) {
		defer seenBy(r)()
		if _, err := restgw.ProcessFilePart(r.Context(), r, restgw.FilePartConfig{
			FormName: "file", Driver: uploads, TmpDir: tmp, BucketPath: "up", MaxSizeBytes: 1 << 20, AllowedExtensions: []string{"txt"},
		}); err != nil {
			restgw.WriteUploadErrorCtx(r.Context(), w, err)
			return
		}
		if err := b.unary(r.Context()); err != nil {
			fail(w, err)
		}
	})
	r.Get("/download/*", func(w http.ResponseWriter, r *http.Request) {
		defer seenBy(r)()
		if err := b.unary(r.Context()); err != nil {
			fail(w, err)
			return
		}
		restgw.ServeDownload(w, r, dl)
	})
	return r
}

func writeMemoryObject(t *testing.T, d *memory.Driver, key string) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "obj")
	if err := os.WriteFile(p, []byte("body"), 0o600); err != nil {
		return err
	}
	_, err := d.PutFromTempFile(context.Background(), key, p)
	return err
}

func TestEdge_ClientCannotSetReservedMetadataOnAnySurface(t *testing.T) {
	b := startRecordingBackend(t)
	handlerHeaders := make(chan http.Header, 1)
	srv := httptest.NewServer(edgeChain(edgeRouter(t, b, handlerHeaders)))
	defer srv.Close()

	send := func(t *testing.T, method, path string, body *bytes.Buffer, contentType string) {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			rdr = bytes.NewReader(body.Bytes())
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, srv.URL+path, rdr)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = forgedHeaders()
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		// Read to the end: closing early cancels the request, and a
		// streaming surface that has already sent its headers would then
		// abandon its backend call before making it.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: status %d", method, path, resp.StatusCode)
		}
	}

	cases := []struct {
		name string
		do   func(t *testing.T)
		// gatewayStamped is what the route's own auth stamps; every other
		// reserved key must be absent.
		gatewayStamped map[string]string
	}{
		{name: "unary without auth", do: func(t *testing.T) { send(t, http.MethodGet, "/public", nil, "") }},
		{
			name: "unary with auth",
			do:   func(t *testing.T) { send(t, http.MethodGet, "/authed", nil, "") },
			gatewayStamped: map[string]string{
				"x-w17-user":            base64.StdEncoding.EncodeToString(gatewayPrincipal),
				"x-w17-scope-tenant_id": "verified-tenant",
			},
		},
		{name: "server-sent events", do: func(t *testing.T) { send(t, http.MethodGet, "/sse", nil, "") }},
		{name: "websocket", do: func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
				&websocket.DialOptions{HTTPHeader: forgedHeaders()})
			if err != nil {
				t.Fatalf("ws dial: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()
			_, _, _ = conn.Read(ctx) // until the server closes
		}},
		{name: "event hub", do: func(t *testing.T) { send(t, http.MethodGet, "/w17-events", nil, "") }},
		{name: "upload", do: func(t *testing.T) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, err := mw.CreateFormFile("file", "a.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write([]byte("hello"))
			_ = mw.Close()
			send(t, http.MethodPost, "/upload", &buf, mw.FormDataContentType())
		}},
		{name: "download", do: func(t *testing.T) { send(t, http.MethodGet, "/download/files/a.txt", nil, "") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.do(t)
			var h http.Header
			select {
			case h = <-handlerHeaders:
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not run")
			}
			md := b.take(t)
			for _, k := range reservedKeys {
				got := md.Get(k)
				want, stamped := tc.gatewayStamped[k]
				switch {
				case stamped && (len(got) != 1 || got[0] != want):
					t.Errorf("backend %s = %q, want only the gateway's %q", k, got, want)
				case !stamped && len(got) != 0:
					t.Errorf("backend %s = %q, want absent: a client header reached a gateway-reserved key", k, got)
				}
			}
			if got := md.Get("x-tenant-hint"); len(got) != 1 || got[0] != "hint" {
				t.Errorf("backend x-tenant-hint = %q, want [hint]: ordinary propagation must keep working", got)
			}
			if got := md.Get("w17-language"); len(got) != 1 || got[0] != "cs" {
				t.Errorf("backend w17-language = %q, want [cs]: ordinary renames must keep working", got)
			}
			if got := md.Get("w17-format-locale"); len(got) != 1 || got[0] != "cs-CZ" {
				t.Errorf("backend w17-format-locale = %q, want [cs-CZ]: ordinary default stamps must keep working", got)
			}
			for name := range h {
				if strings.HasPrefix(strings.ToLower(name), "x-w17-") || strings.HasPrefix(strings.ToLower(name), "w17-label-") ||
					strings.EqualFold(name, "W17-Tx-Id") || strings.EqualFold(name, "W17-Conn-Id") ||
					strings.HasPrefix(strings.ToLower(name), "grpcgateway-") || strings.HasPrefix(strings.ToLower(name), "grpc-metadata-") {
					t.Errorf("handler saw client header %s: the inbound scrub must remove it before any layer reads it", name)
				}
			}
			if h.Get("X-Tenant-Hint") != "hint" {
				t.Errorf("handler lost an ordinary header: %v", h)
			}
		})
	}
}

// The middlewares refuse a reserved key on their own, without the inbound
// scrub in front: the SDK cannot check how a hand-written gateway mounts it,
// and a rename can route an innocuous header into a reserved key, which no
// header scrub sees.
func TestMetadataMiddlewares_RefuseReservedKeysWithoutTheScrub(t *testing.T) {
	var got metadata.MD
	sink := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = metadata.FromOutgoingContext(r.Context())
	})
	h := restgw.MetadataPropagationMiddleware([]string{"X-W17-User", "W17-Tx-Id", "X-Tenant-Hint"},
		restgw.MetadataRenameMiddleware([]restgw.HeaderRenameRule{{HTTP: "X-Org", Metadata: "x-w17-scope-org_id"}},
			restgw.MetadataDefaultStampMiddleware([]restgw.DefaultMetadataStamp{{Key: "w17-label-tenant", Value: "x"}}, sink)))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header = forgedHeaders()
	h.ServeHTTP(httptest.NewRecorder(), req)
	for _, k := range []string{"x-w17-user", "w17-tx-id", "x-w17-scope-org_id", "w17-label-tenant"} {
		if v := got.Get(k); len(v) != 0 {
			t.Errorf("outgoing %s = %q, want absent", k, v)
		}
	}
	if v := got.Get("x-tenant-hint"); len(v) != 1 {
		t.Errorf("outgoing x-tenant-hint = %q, want one value (decoy)", v)
	}
}

// A list made only of reserved entries leaves nothing to do: the
// middlewares return next itself rather than a wrapper that forwards
// nothing.
func TestMetadataMiddlewares_AllReservedIsPassThrough(t *testing.T) {
	next := &passMarker{}
	same := func(h http.Handler) bool { p, ok := h.(*passMarker); return ok && p == next }
	if !same(restgw.MetadataPropagationMiddleware([]string{"x-w17-user"}, next)) {
		t.Error("propagation of only reserved headers should return next")
	}
	if !same(restgw.MetadataRenameMiddleware([]restgw.HeaderRenameRule{{HTTP: "A", Metadata: "x-w17-user"}}, next)) {
		t.Error("renames into only reserved keys should return next")
	}
	if !same(restgw.MetadataDefaultStampMiddleware([]restgw.DefaultMetadataStamp{{Key: "w17-conn-id", Value: "x"}}, next)) {
		t.Error("stamps of only reserved keys should return next")
	}
}

type passMarker struct{}

func (*passMarker) ServeHTTP(http.ResponseWriter, *http.Request) {}
