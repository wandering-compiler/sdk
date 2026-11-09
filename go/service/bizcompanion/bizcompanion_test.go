package bizcompanion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	filespb "github.com/wandering-compiler/sdk/go/pb/common/files"
)

// descriptorSet compiles testdata/echo.proto and writes it, with
// common/files/files.proto and every import, as a descriptor set — what
// codegen writes beside the manifest.
func descriptorSet(t *testing.T, dir string) {
	t.Helper()
	src, err := os.ReadFile("testdata/echo.proto")
	if err != nil {
		t.Fatal(err)
	}
	const path = "clients/echo/echo.proto"
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(protocompile.ResolverFunc(func(p string) (protocompile.SearchResult, error) {
		if p == path {
			return protocompile.SearchResult{Source: bytes.NewReader(src)}, nil
		}
		fd, err := protoregistry.GlobalFiles.FindFileByPath(p)
		if err != nil {
			return protocompile.SearchResult{}, err
		}
		return protocompile.SearchResult{Desc: fd}, nil
	}))}
	files, err := c.Compile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imports := fd.Imports()
		for i := 0; i < imports.Len(); i++ {
			add(imports.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	add(files[0])
	add(filespb.File_common_files_files_proto)
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "descriptors.binpb"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// companionSection is the manifest's companion section of the tests.
func companionSection() map[string]any {
	return map[string]any{
		"address_env":     "NOTES_BUSINESS_COMPANION_BACKEND_ADDR",
		"default_address": "notes-business-companion:50051",
		"egress": []any{map[string]any{
			"client": "echo", "service": "clients.echo.EchoApi", "dsn_env": "W17_CLIENT_ECHO_DSN",
		}},
		"files": map[string]any{
			"service": FilesService,
			"connections": []any{
				map[string]any{"name": "notes-exports", "root_env": "NOTES_GATEWAY_UPLOAD_NOTES_EXPORTS_ROOT", "default_root": "/var/lib/w17/uploads/notes-exports", "writable": true},
				map[string]any{"name": "notes-media", "root_env": "NOTES_GATEWAY_UPLOAD_NOTES_MEDIA_ROOT", "default_root": "/var/lib/w17/uploads/notes-media", "writable": false},
			},
			"placements": []any{map[string]any{
				"field": "app.notes.NoteExport.file", "name": "NoteExportFile", "connection": "notes-exports",
				"bucket_path": "/note-exports", "expected_objects": uint64(18446744073709551615), "max_per_bucket": 100, "max_size_bytes": 65536,
			}},
		},
	}
}

// writeManifest writes a manifest (format, companion; the business keys
// trimmed to what the companion reads plus one it ignores) and the
// descriptor set into a temp dir and returns the manifest's path.
func writeManifest(t *testing.T, format any, companion any) string {
	t.Helper()
	dir := t.TempDir()
	descriptorSet(t, dir)
	m := map[string]any{
		"format": format, "domain": "notes", "bundle": "notes-business", "language": "python",
		"env_prefix": "NOTES", "descriptor_set": "descriptors.binpb", "plugin_hooks": []any{},
		"companion": companion,
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "w17-business.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	cfg, err := Load(writeManifest(t, 2, companionSection()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Format != 2 || cfg.Domain != "notes" || cfg.EnvPrefix != "NOTES" || cfg.Companion == nil {
		t.Fatalf("config %+v", cfg)
	}
	c := cfg.Companion
	if len(c.Egress) != 1 || c.Egress[0].descriptor == nil || c.Egress[0].descriptor.FullName() != "clients.echo.EchoApi" {
		t.Fatalf("egress %+v", c.Egress)
	}
	fs := c.FileStorage
	if fs == nil || len(fs.Connections) != 2 || len(fs.Placements) != 1 {
		t.Fatalf("files %+v", fs)
	}
	// 64-bit counts are read exactly.
	if p := fs.Placements[0]; p.ExpectedObjects != 18446744073709551615 || p.MaxPerBucket != 100 || p.MaxSizeBytes != 65536 || p.BucketPath != "/note-exports" {
		t.Fatalf("placement %+v", p)
	}
	if ListenEnv(cfg) != "NOTES_BUSINESS_COMPANION_LISTEN_ADDR" || ServiceName(cfg) != "notes-business-companion" {
		t.Fatalf("names %s %s", ListenEnv(cfg), ServiceName(cfg))
	}
	if got := ListenAddr(cfg, func(string) string { return "" }); got != ":50051" {
		t.Fatalf("default listen %s", got)
	}

	// A format-1 manifest (no companion) loads, with nothing to serve.
	cfg, err = Load(writeManifest(t, 1, nil))
	if err != nil || cfg.Companion != nil {
		t.Fatalf("format 1: %+v, %v", cfg, err)
	}
	if _, err := Register(context.Background(), grpc.NewServer(), cfg, nil); err == nil {
		t.Fatal("Register served a manifest without a companion")
	}
	// A format-2 manifest without files: egress only.
	only := companionSection()
	only["files"] = nil
	cfg, err = Load(writeManifest(t, 2, only))
	if err != nil || cfg.Companion.FileStorage != nil {
		t.Fatalf("no files: %+v, %v", cfg, err)
	}
}

func TestLoadRefusals(t *testing.T) {
	edit := func(f func(c map[string]any)) map[string]any {
		c := companionSection()
		f(c)
		return c
	}
	files := func(c map[string]any) map[string]any { return c["files"].(map[string]any) }
	for name, tc := range map[string]struct {
		format    any
		companion any
		want      string
	}{
		"format 3":              {3, companionSection(), "format 3 is not one this companion reads"},
		"format 0":              {0, nil, "format 0 is not one"},
		"no format":             {nil, nil, "format none is not one"},
		"format 1 with section": {1, companionSection(), "format 1 has no companion section"},
		"unknown service": {2, edit(func(c map[string]any) {
			c["egress"].([]any)[0].(map[string]any)["service"] = "clients.nope.NopeApi"
		}), "clients.nope.NopeApi is not in the descriptor set"},
		"not a service": {2, edit(func(c map[string]any) {
			c["egress"].([]any)[0].(map[string]any)["service"] = "clients.echo.Echo"
		}), "is not a service"},
		"egress without env": {2, edit(func(c map[string]any) {
			c["egress"].([]any)[0].(map[string]any)["dsn_env"] = ""
		}), "dsn_env are required"},
		"wrong files service": {2, edit(func(c map[string]any) { files(c)["service"] = "x.Files" }), "the companion serves w17.common.files.W17Files"},
		"placement on unknown connection": {2, edit(func(c map[string]any) {
			files(c)["placements"].([]any)[0].(map[string]any)["connection"] = "nope"
		}), "which is not in files.connections"},
		"connection twice": {2, edit(func(c map[string]any) {
			cs := files(c)["connections"].([]any)
			files(c)["connections"] = append(cs, cs[0])
		}), "notes-exports is listed twice"},
		"connection without root_env": {2, edit(func(c map[string]any) {
			files(c)["connections"].([]any)[0].(map[string]any)["root_env"] = ""
		}), "root_env are required"},
		"empty section":     {2, map[string]any{}, "address_env is required"},
		"empty address_env": {2, edit(func(c map[string]any) { c["address_env"] = "" }), "address_env is required"},
		"placement twice": {2, edit(func(c map[string]any) {
			ps := files(c)["placements"].([]any)
			files(c)["placements"] = append(ps, ps[0])
		}), "app.notes.NoteExport.file is listed twice"},
		"egress client twice": {2, edit(func(c map[string]any) {
			es := c["egress"].([]any)
			c["egress"] = append(es, es[0])
		}), "client echo is listed twice"},
		"egress service twice": {2, edit(func(c map[string]any) {
			es := c["egress"].([]any)
			c["egress"] = append(es, map[string]any{"client": "echo2", "service": "clients.echo.EchoApi", "dsn_env": "W17_CLIENT_ECHO2_DSN"})
		}), "service clients.echo.EchoApi is listed twice"},
		"writable as a string": {2, edit(func(c map[string]any) {
			files(c)["connections"].([]any)[1].(map[string]any)["writable"] = "false"
		}), "writable"},
	} {
		_, err := Load(writeManifest(t, tc.format, tc.companion))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing manifest loaded")
	}
	path := writeManifest(t, 2, companionSection())
	if err := os.Remove(filepath.Join(filepath.Dir(path), "descriptors.binpb")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "descriptor set") {
		t.Errorf("a missing descriptor set: %v", err)
	}
}

// upstream is the fake third-party API: GET /echo/{id} answers JSON,
// /echo/missing 404. It records the headers of each request.
func upstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/echo/")
		if id == "missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no such echo"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "message": "echo " + id + " auth=" + r.Header.Get("Authorization")})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type served struct {
	cc  *grpc.ClientConn
	cfg *Config
	md  chan metadata.MD
}

// serveCompanion registers the companion of manifest on a real gRPC server
// over an in-memory listener; an interceptor records each call's incoming
// metadata.
func serveCompanion(t *testing.T, path string, env map[string]string) *served {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mds := make(chan metadata.MD, 16)
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		mds <- md
		return h(ctx, req)
	}))
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	closer, err := Register(context.Background(), srv, cfg, lookup)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		if err := closer(context.Background()); err != nil {
			t.Error(err)
		}
	})
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return &served{cc: cc, cfg: cfg, md: mds}
}

// getEcho calls clients.echo.EchoApi/GetEcho with dynamic messages, as a
// client that only has the descriptor set does.
func (s *served) getEcho(ctx context.Context, id string) (*dynamicpb.Message, error) {
	svc := s.cfg.Companion.Egress[0].descriptor
	md := svc.Methods().ByName("GetEcho")
	in := dynamicpb.NewMessage(md.Input())
	in.Set(md.Input().Fields().ByName("id"), protoreflect.ValueOfString(id))
	out := dynamicpb.NewMessage(md.Output())
	err := s.cc.Invoke(ctx, "/clients.echo.EchoApi/GetEcho", in, out)
	return out, err
}

func setRoots(t *testing.T) (exports, media string) {
	exports = filepath.Join(t.TempDir(), "exports") // created by the companion: writable
	media = t.TempDir()                             // must exist: read-only
	t.Setenv("NOTES_GATEWAY_UPLOAD_NOTES_EXPORTS_ROOT", exports)
	t.Setenv("NOTES_GATEWAY_UPLOAD_NOTES_MEDIA_ROOT", media)
	return exports, media
}

func errorInfo(t *testing.T, err error) *errdetails.ErrorInfo {
	t.Helper()
	for _, d := range status.Convert(err).Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			return i
		}
	}
	t.Fatalf("%v carries no ErrorInfo", err)
	return nil
}

func TestRegisterServesEgressAndFiles(t *testing.T) {
	up := upstream(t)
	exports, _ := setRoots(t)
	s := serveCompanion(t, writeManifest(t, 2, companionSection()), map[string]string{
		"W17_CLIENT_ECHO_DSN": up.URL + "?auth=bearer&token=t0k",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Egress: a client loaded from the descriptor set calls the upstream,
	// and the call's metadata reaches the companion as incoming metadata.
	out, err := s.getEcho(metadata.AppendToOutgoingContext(ctx, "x-w17-user", "u-1"), "42")
	if err != nil {
		t.Fatal(err)
	}
	msg := out.Get(out.Descriptor().Fields().ByName("message")).String()
	if msg != "echo 42 auth=Bearer t0k" {
		t.Fatalf("upstream answered %q", msg)
	}
	if md := <-s.md; len(md.Get("x-w17-user")) != 1 || md.Get("x-w17-user")[0] != "u-1" {
		t.Fatalf("incoming metadata %v", md)
	}
	// Its errors are the egress runtime's.
	_, err = s.getEcho(ctx, "missing")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
	if info := errorInfo(t, err); info.GetDomain() != "echo" || info.GetReason() != "UPSTREAM_HTTP_404" {
		t.Fatalf("ErrorInfo %v", info)
	}
	<-s.md

	// Files: the writable root was created; a put lands under it.
	files := filespb.NewW17FilesClient(s.cc)
	st, err := files.Put(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_Header{Header: &filespb.PutHeader{Field: "app.notes.NoteExport.file"}}})
	_ = st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_Chunk{Chunk: []byte("hello")}})
	_ = st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_End{End: &filespb.PutEnd{Size: 5}}})
	resp, err := st.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.GetHandle(), "note-exports/") {
		t.Fatalf("handle %q", resp.GetHandle())
	}
	if b, err := os.ReadFile(filepath.Join(exports, filepath.FromSlash(resp.GetHandle()))); err != nil || string(b) != "hello" {
		t.Fatalf("stored %q, %v", b, err)
	}
	// The read-only connection refuses a delete.
	_, err = files.Delete(ctx, &filespb.DeleteRequest{Connection: "notes-media", Handle: "x"})
	if status.Code(err) != codes.FailedPrecondition || errorInfo(t, err).GetReason() != "READ_ONLY_CONNECTION" {
		t.Fatalf("delete on read-only: %v", err)
	}
	// Too large for the placement.
	st, err = files.Put(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_Header{Header: &filespb.PutHeader{Field: "app.notes.NoteExport.file"}}})
	_ = st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_Chunk{Chunk: make([]byte, 65537)}})
	_, err = st.CloseAndRecv()
	if info := errorInfo(t, err); info.GetReason() != "FILE_TOO_LARGE" || info.GetMetadata()["limit_bytes"] != "65536" {
		t.Fatalf("too large: %v", err)
	}
}

func TestRegisterUnconfiguredClient(t *testing.T) {
	setRoots(t)
	s := serveCompanion(t, writeManifest(t, 2, companionSection()), nil)
	_, err := s.getEcho(context.Background(), "1")
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "W17_CLIENT_ECHO_DSN") {
		t.Fatalf("got %v", err)
	}
	if info := errorInfo(t, err); info.GetReason() != "CLIENT_UNCONFIGURED" || info.GetMetadata()["env"] != "W17_CLIENT_ECHO_DSN" {
		t.Fatalf("ErrorInfo %v", info)
	}
}

func TestRegisterBootRefusals(t *testing.T) {
	cfg, err := Load(writeManifest(t, 2, companionSection()))
	if err != nil {
		t.Fatal(err)
	}
	register := func(env map[string]string) error {
		_, err := Register(context.Background(), grpc.NewServer(), cfg, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		return err
	}

	setRoots(t)
	// A DSN that does not parse fails the boot naming the variable, not the value.
	err = register(map[string]string{"W17_CLIENT_ECHO_DSN": "https://api.example?auth=bearer&tokn=s3cr3t"})
	if err == nil || !strings.Contains(err.Error(), "W17_CLIENT_ECHO_DSN") || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("bad DSN: %v", err)
	}

	// An unset root.
	t.Setenv("NOTES_GATEWAY_UPLOAD_NOTES_MEDIA_ROOT", "")
	if err := register(nil); err == nil || !strings.Contains(err.Error(), "NOTES_GATEWAY_UPLOAD_NOTES_MEDIA_ROOT is required") {
		t.Fatalf("unset root: %v", err)
	}
	// A read-only root that does not exist is not created.
	missing := filepath.Join(t.TempDir(), "not-there")
	t.Setenv("NOTES_GATEWAY_UPLOAD_NOTES_MEDIA_ROOT", missing)
	if err := register(nil); err == nil || !strings.Contains(err.Error(), "notes-media") {
		t.Fatalf("missing read-only root: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the read-only root was created: %v", err)
	}
	// A Config not built by Load.
	if _, err := Register(context.Background(), grpc.NewServer(), &Config{Companion: &Companion{Egress: []EgressEntry{{Client: "x"}}}}, nil); err == nil {
		t.Fatal("an unresolved egress entry registered")
	}
}

// TestMainServesAndProbes runs the standalone server the way Main does,
// probes it with the health subcommand and grpc.health.v1, calls it, and
// stops it.
func TestMainServesAndProbes(t *testing.T) {
	up := upstream(t)
	exports, media := setRoots(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	path := writeManifest(t, 2, companionSection())
	env := map[string]string{
		ManifestEnv:                               path,
		"NOTES_BUSINESS_COMPANION_LISTEN_ADDR":    addr,
		"NOTES_GATEWAY_UPLOAD_NOTES_EXPORTS_ROOT": exports,
		"NOTES_GATEWAY_UPLOAD_NOTES_MEDIA_ROOT":   media,
	}
	getenv := func(k string) string { return env[k] }
	t.Setenv("W17_CLIENT_ECHO_DSN", up.URL) // the secret chain reads the environment first

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, getenv) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("run did not stop")
		}
	}()

	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, stop := context.WithTimeout(context.Background(), time.Second)
		resp, err := healthpb.NewHealthClient(cc).Check(c, &healthpb.HealthCheckRequest{})
		stop()
		if err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the companion never served: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := run(context.Background(), []string{"health"}, getenv); err != nil {
		t.Fatalf("health: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &served{cc: cc, cfg: cfg}
	c, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if _, err := s.getEcho(c, "7"); err != nil {
		t.Fatalf("egress through the standalone companion: %v", err)
	}
}

func TestMainRefusals(t *testing.T) {
	if err := run(context.Background(), nil, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), ManifestEnv) {
		t.Fatalf("no manifest: %v", err)
	}
	path := writeManifest(t, 1, nil)
	err := run(context.Background(), nil, func(k string) string {
		if k == ManifestEnv {
			return path
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "needs no companion") {
		t.Fatalf("format 1: %v", err)
	}
}
