// Package conformance proves, on the wire, that a business SDK in another
// language keeps sdk/go's semantics.
//
// The harness is Go and real: a storage gRPC server with the distributed-
// transaction coordinator (distx.Server over txregistry.Memory over SQLite),
// adopting transactions as generated storage handlers do; an embedded NATS
// JetStream broker consumed through sdk/go's own eventbus subscriber; and a
// client that calls the business tier under test like a gateway would,
// stamping the principal metadata a gateway stamps; and the tier's business
// companion — sdk/go/service/bizcompanion from this checkout, over the
// suite's manifest — in front of a fake third-party API (the echo egress
// client's upstream) and two temp-dir file stores.
//
// The tier under test is the fixed NoteService of
// proto/conformance/v1/conformance.proto, implemented once per language
// (service/ here for the Rust SDK) and run in its own container. The harness
// serves storage on :50051, NATS on :4222 and the companion on :50071 (its
// upstream on 127.0.0.1:50072, $W17_CLIENT_ECHO_DSN), and reaches the tier at
// $CONFORMANCE_BUSINESS_ADDR; without that variable every test skips, so
// `go test ./...` here is harmless. `make conformance-rs` runs the whole
// thing in docker.
//
// The messages are built from testdata/descriptors.binpb with dynamicpb: the
// harness reads the same descriptor set the SDK under test reads, and needs
// no generated code of its own.
package conformance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/paging"
	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"github.com/wandering-compiler/sdk/go/service/bizcompanion"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

const (
	envBusinessAddr = "CONFORMANCE_BUSINESS_ADDR"
	storageListen   = "0.0.0.0:50051"
	natsPort        = 4222
	pkg             = "conformance.v1."
	// The companion the tier dials (NOTES_COMPANION_ADDR), and the fake
	// upstream of the echo egress client (W17_CLIENT_ECHO_DSN names it).
	companionListen = "0.0.0.0:50071"
	upstreamListen  = "127.0.0.1:50072"
	// The suite's upload connections' root variables (the manifest's
	// companion.files.connections).
	envFilesRoot = "NOTES_GATEWAY_UPLOAD_NOTES_FILES_ROOT"
	envMediaRoot = "NOTES_GATEWAY_UPLOAD_NOTES_MEDIA_ROOT"
)

// env is the one harness every test shares: the tier under test boots once
// against it.
var env *harness

type harness struct {
	files    *protoregistry.Files
	db       *sql.DB
	reg      *txregistry.Memory
	business *grpc.ClientConn
	natsURL  string

	mu sync.Mutex
	// inspected is every Inspect call storage served, newest last.
	inspected []metadata.MD
	// listed is the metadata of every ListNotes call.
	listed []metadata.MD
	// dropCommitReply makes the next Commit commit and then answer
	// Unavailable, as a reply lost in transport.
	dropCommitReply bool
	events          []event

	// filesRoot is the writable store's directory.
	filesRoot string
	// companionCalls is every call the companion served, oldest first.
	companionCalls []companionCall
}

// companionCall is what one companion call carried.
type companionCall struct {
	method string
	md     metadata.MD
	// messages is the number of messages a client stream sent (Put).
	messages int
	// existed reports, for a Delete, whether the object was on disk under
	// the store root when the call arrived.
	existed bool
	// err is what a client stream's handler returned (Put).
	err error
}

type event struct {
	topic string
	raw   []byte
}

func TestMain(m *testing.M) {
	addr := os.Getenv(envBusinessAddr)
	if addr == "" {
		fmt.Fprintf(os.Stderr, "conformance: %s is not set; skipping (run `make conformance-rs`)\n", envBusinessAddr)
		os.Exit(0)
	}
	h, cleanup, err := start(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		os.Exit(1)
	}
	env = h
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func start(businessAddr string) (*harness, func(), error) {
	dir, err := os.MkdirTemp("", "conformance-")
	if err != nil {
		return nil, nil, err
	}
	var closers []func()
	cleanup := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
		_ = os.RemoveAll(dir)
	}
	h := &harness{}

	raw, err := os.ReadFile(filepath.Join("testdata", "descriptors.binpb"))
	if err != nil {
		return nil, cleanup, err
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, cleanup, err
	}
	if h.files, err = protodesc.NewFiles(&set); err != nil {
		return nil, cleanup, fmt.Errorf("descriptor set: %w", err)
	}

	// A file on disk: ":memory:" would give every pooled connection its own
	// database.
	if h.db, err = sql.Open("sqlite", filepath.Join(dir, "notes.db")+"?_pragma=busy_timeout(5000)"); err != nil {
		return nil, cleanup, err
	}
	closers = append(closers, func() { _ = h.db.Close() })
	if _, err := h.db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY AUTOINCREMENT, text TEXT NOT NULL)`); err != nil {
		return nil, cleanup, err
	}
	h.reg = txregistry.NewMemory(map[string]*sql.DB{"main": h.db})

	ns, err := natsserver.NewServer(&natsserver.Options{
		Host: "0.0.0.0", Port: natsPort, JetStream: true, StoreDir: filepath.Join(dir, "js"), NoLog: true, NoSigs: true,
	})
	if err != nil {
		return nil, cleanup, err
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		return nil, cleanup, fmt.Errorf("nats did not start")
	}
	closers = append(closers, ns.Shutdown)
	h.natsURL = fmt.Sprintf("nats://127.0.0.1:%d", natsPort)

	// sdk/go's own subscriber consumes what the tier publishes. It ensures the
	// channel's stream with Go's configuration BEFORE the tier's first
	// publish, so the tier's create-or-update must accept Go's stream as is.
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: h.natsURL, DurablePrefix: "conformance"})
	if err != nil {
		return nil, cleanup, err
	}
	closers = append(closers, func() { _ = bus.Close(context.Background()) })
	sub, err := bus.Subscriber(context.Background(), "notes")
	if err != nil {
		return nil, cleanup, err
	}
	// EveryTopic: the tier publishes the reserved w17.invalidate too, which
	// a `**` subscription leaves out.
	if err := sub.Subscribe(context.Background(), eventbus.EveryTopic, func(_ context.Context, topic string, envelope []byte) error {
		h.mu.Lock()
		h.events = append(h.events, event{topic: topic, raw: append([]byte(nil), envelope...)})
		h.mu.Unlock()
		return nil
	}); err != nil {
		return nil, cleanup, err
	}

	gs := grpc.NewServer(grpc.UnaryInterceptor(h.interceptor))
	distxpb.RegisterW17DistributedTransactionServer(gs, distx.NewServer(h.reg))
	gs.RegisterService(h.storageDesc(), h)
	lis, err := net.Listen("tcp", storageListen)
	if err != nil {
		return nil, cleanup, err
	}
	go func() { _ = gs.Serve(lis) }()
	closers = append(closers, gs.Stop)

	if err := h.startCompanion(dir, &closers); err != nil {
		return nil, cleanup, fmt.Errorf("companion: %w", err)
	}

	if h.business, err = grpc.NewClient(businessAddr, grpc.WithTransportCredentials(insecure.NewCredentials())); err != nil {
		return nil, cleanup, err
	}
	closers = append(closers, func() { _ = h.business.Close() })
	// The runner starts the tier under test on this line: it dials storage
	// and NATS at boot, so they have to be up first.
	fmt.Fprintln(os.Stderr, "conformance: harness ready")
	if err := waitServing(h.business, 120*time.Second); err != nil {
		return nil, cleanup, fmt.Errorf("business tier at %s: %w", businessAddr, err)
	}
	return h, cleanup, nil
}

// startCompanion runs the tier's business companion as the split bundle's
// main would, from the suite's manifest: its egress client against a fake
// upstream, its file stores in temp dirs. An interceptor records what each
// call carried.
func (h *harness) startCompanion(dir string, closers *[]func()) error {
	up, err := net.Listen("tcp", upstreamListen)
	if err != nil {
		return err
	}
	upstream := &http.Server{Handler: http.HandlerFunc(echoUpstream), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = upstream.Serve(up) }()
	*closers = append(*closers, func() { _ = upstream.Close() })
	if os.Getenv("W17_CLIENT_ECHO_DSN") == "" {
		return fmt.Errorf("W17_CLIENT_ECHO_DSN is not set (run.sh sets it to http://%s?…)", upstreamListen)
	}

	// The writable root is created by the companion; the read-only one must
	// exist, as the gateway (its writer) would have made it.
	h.filesRoot = filepath.Join(dir, "notes-files")
	media := filepath.Join(dir, "notes-media")
	if err := os.MkdirAll(media, 0o750); err != nil {
		return err
	}
	if err := os.Setenv(envFilesRoot, h.filesRoot); err != nil {
		return err
	}
	if err := os.Setenv(envMediaRoot, media); err != nil {
		return err
	}
	cfg, err := bizcompanion.Load(filepath.Join("service", "w17-business.json"))
	if err != nil {
		return err
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(h.companionUnary), grpc.ChainStreamInterceptor(h.companionStream))
	closer, err := bizcompanion.Register(context.Background(), srv, cfg, os.LookupEnv)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", companionListen)
	if err != nil {
		return err
	}
	go func() { _ = srv.Serve(lis) }()
	*closers = append(*closers, func() {
		srv.Stop()
		_ = closer(context.Background())
	})
	return nil
}

// echoUpstream is the echo client's third-party API: GET /echo/{id} answers
// {"id", "message"} (the message naming the credential the call carried),
// /echo/missing 404, /echo/slow after 2 s.
func echoUpstream(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/echo/")
	switch id {
	case "missing":
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such echo"}`))
		return
	case "slow":
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "message": "echo " + id + " auth=" + r.Header.Get("Authorization")})
}

func (h *harness) recordCompanion(c companionCall) {
	h.mu.Lock()
	h.companionCalls = append(h.companionCalls, c)
	h.mu.Unlock()
}

func (h *harness) companionUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	c := companionCall{method: info.FullMethod, md: md.Copy()}
	if del, ok := req.(interface{ GetHandle() string }); ok && strings.HasSuffix(info.FullMethod, "/Delete") {
		_, err := os.Stat(filepath.Join(h.filesRoot, filepath.FromSlash(del.GetHandle())))
		c.existed = err == nil
	}
	h.recordCompanion(c)
	return handler(ctx, req)
}

func (h *harness) companionStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	md, _ := metadata.FromIncomingContext(ss.Context())
	counted := &countingStream{ServerStream: ss}
	err := handler(srv, counted)
	h.recordCompanion(companionCall{method: info.FullMethod, md: md.Copy(), messages: counted.n, err: err})
	return err
}

type countingStream struct {
	grpc.ServerStream
	n int
}

func (s *countingStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil {
		s.n++
	}
	return err
}

// companionSince is the companion calls after the first mark ones.
func (h *harness) companionSince(mark int) []companionCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]companionCall(nil), h.companionCalls[mark:]...)
}

func (h *harness) companionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.companionCalls)
}

func waitServing(cc *grpc.ClientConn, d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := grpc_health_v1.NewHealthClient(cc).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		cancel()
		if err == nil && resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING {
			return nil
		}
		last = err
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("not SERVING after %s: %v", d, last)
}

// interceptor stands in for the storage proxy's view: it records what Inspect
// and ListNotes carried, and loses a Commit's reply on request.
func (h *harness) interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if strings.HasSuffix(info.FullMethod, "/Commit") {
		h.mu.Lock()
		drop := h.dropCommitReply
		h.dropCommitReply = false
		h.mu.Unlock()
		if drop && err == nil {
			// Committed, and the caller hears Unavailable: the reply was
			// lost. The response headers go out first, as they do when a
			// connection fails mid-reply: that makes the call COMMITTED for
			// the client's retry policy (gRPC A6), so it is not retried —
			// a trailers-only Unavailable would be, in every language.
			_ = grpc.SendHeader(ctx, metadata.MD{})
			return nil, status.Error(codes.Unavailable, "conformance: commit reply dropped")
		}
	}
	return resp, err
}

// --- messages ----------------------------------------------------------------

func (h *harness) desc(name string) protoreflect.MessageDescriptor {
	d, err := h.files.FindDescriptorByName(protoreflect.FullName(pkg + name))
	if err != nil {
		panic(err)
	}
	return d.(protoreflect.MessageDescriptor)
}

func (h *harness) msg(name string) *dynamicpb.Message { return dynamicpb.NewMessage(h.desc(name)) }

// set assigns scalar fields by name.
func set(m *dynamicpb.Message, kv map[string]any) *dynamicpb.Message {
	fields := m.Descriptor().Fields()
	for k, v := range kv {
		fd := fields.ByName(protoreflect.Name(k))
		if fd == nil {
			panic("no field " + k)
		}
		if fd.IsList() {
			l := m.Mutable(fd).List()
			for _, s := range v.([]string) {
				l.Append(protoreflect.ValueOfString(s))
			}
			continue
		}
		m.Set(fd, protoreflect.ValueOf(v))
	}
	return m
}

func get(m protoreflect.Message, name string) protoreflect.Value {
	return m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(name)))
}

// call invokes a business method; resp is a fresh dynamic message.
func (h *harness) call(ctx context.Context, method string, req proto.Message, respType string, opts ...grpc.CallOption) (*dynamicpb.Message, error) {
	resp := h.msg(respType)
	err := h.business.Invoke(ctx, "/conformance.v1.NoteService/"+method, req, resp, opts...)
	return resp, err
}

// --- storage -------------------------------------------------------------------

func (h *harness) storageDesc() *grpc.ServiceDesc {
	unary := func(name, reqType string, fn func(ctx context.Context, req *dynamicpb.Message) (proto.Message, error)) grpc.MethodDesc {
		return grpc.MethodDesc{
			MethodName: name,
			Handler: func(_ any, ctx context.Context, dec func(any) error, icpt grpc.UnaryServerInterceptor) (any, error) {
				req := h.msg(reqType)
				if err := dec(req); err != nil {
					return nil, err
				}
				info := &grpc.UnaryServerInfo{FullMethod: "/conformance.v1.NoteStorage/" + name}
				run := func(ctx context.Context, r any) (any, error) { return fn(ctx, r.(*dynamicpb.Message)) }
				if icpt == nil {
					return run(ctx, req)
				}
				return icpt(ctx, req, info, run)
			},
		}
	}
	return &grpc.ServiceDesc{
		ServiceName: "conformance.v1.NoteStorage",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			unary("InsertNote", "InsertNoteRequest", h.insertNote),
			unary("ListNotes", "ListNotesRequest", h.listNotes),
			unary("Inspect", "InspectRequest", h.inspect),
		},
	}
}

// insertNote adopts the caller's transaction exactly as a generated storage
// handler does (txregistry.AdoptTx on the incoming w17-tx-id).
func (h *harness) insertNote(ctx context.Context, req *dynamicpb.Message) (proto.Message, error) {
	text := get(req, "text").String()
	tx, adopted, release, err := txregistry.AdoptTx(ctx, h.reg, "main")
	if err != nil {
		return nil, err
	}
	defer release()
	var res sql.Result
	if adopted {
		res, err = tx.ExecContext(ctx, `INSERT INTO notes (text) VALUES (?)`, text)
	} else {
		res, err = h.db.ExecContext(ctx, `INSERT INTO notes (text) VALUES (?)`, text)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "insert: %v", err)
	}
	id, _ := res.LastInsertId()
	return set(h.msg("Note"), map[string]any{"id": id, "text": text}), nil
}

// listNotes is a keyset-paged query over the id, as generated storage runs
// it: the page from the incoming paging metadata (a backward resume flips the
// ORDER BY, so its rows come back reversed), and the page's POSITION on the
// response headers (paging.SetPosition) — the client-order last and first
// row's id, and the total. Storage mints no cursor; the gateway does, from the
// position the business method relays.
func (h *harness) listNotes(ctx context.Context, _ *dynamicpb.Message) (proto.Message, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	h.mu.Lock()
	h.listed = append(h.listed, md.Copy())
	h.mu.Unlock()
	limit, ok := paging.LimitFromIncomingMD(ctx)
	if !ok {
		limit = 50
	}
	q := `SELECT id, text FROM notes ORDER BY id LIMIT ?`
	args := []any{limit}
	backward := false
	if b, ok := paging.BoundariesFromIncomingMD(ctx); ok {
		backward = b.Direction == w17pb.Direction_DIRECTION_BACKWARD
		if backward {
			q = `SELECT id, text FROM notes WHERE id < ? ORDER BY id DESC LIMIT ?`
		} else {
			q = `SELECT id, text FROM notes WHERE id > ? ORDER BY id LIMIT ?`
		}
		args = []any{b.Values[0].GetIntV(), limit}
	}
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	resp := h.msg("ListNotesResponse")
	list := resp.Mutable(resp.Descriptor().Fields().ByName("notes")).List()
	var ids []int64
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		list.Append(protoreflect.ValueOfMessage(set(h.msg("Note"), map[string]any{"id": id, "text": text})))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var total uint64
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes`).Scan(&total); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		first, last := ids[0], ids[len(ids)-1]
		if backward {
			first, last = last, first
		}
		paging.SetPosition(ctx, []*w17pb.KeysetValue{paging.FromInt64(last)}, []*w17pb.KeysetValue{paging.FromInt64(first)}, total)
	} else {
		paging.SetPosition(ctx, nil, nil, total)
	}
	pgRaw, _ := proto.Marshal(&w17pb.Paging{Limit: limit, Total: total})
	pgMsg := resp.Mutable(resp.Descriptor().Fields().ByName("paging")).Message()
	if err := proto.Unmarshal(pgRaw, pgMsg.Interface()); err != nil {
		return nil, err
	}
	return resp, nil
}

func (h *harness) inspect(ctx context.Context, _ *dynamicpb.Message) (proto.Message, error) {
	// A sibling call reporting a position of its own: a business tier that
	// relays it instead of the forwarded call's would move the page.
	paging.SetPosition(ctx, []*w17pb.KeysetValue{paging.FromInt64(999)}, nil, 999)
	md, _ := metadata.FromIncomingContext(ctx)
	h.mu.Lock()
	h.inspected = append(h.inspected, md.Copy())
	h.mu.Unlock()
	resp := h.msg("InspectResponse")
	headers := resp.Mutable(resp.Descriptor().Fields().ByName("headers")).List()
	for k, vs := range md {
		hd := set(h.msg("Header"), map[string]any{"key": k, "values": vs})
		headers.Append(protoreflect.ValueOfMessage(hd))
	}
	if dl, ok := ctx.Deadline(); ok {
		set(resp, map[string]any{"has_deadline": true, "deadline_ms": time.Until(dl).Milliseconds()})
	}
	return resp, nil
}

// --- helpers for the tests ---------------------------------------------------

func (h *harness) rows(t *testing.T) []string {
	t.Helper()
	rs, err := h.db.Query(`SELECT text FROM notes ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	var out []string
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (h *harness) eventCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events)
}

func (h *harness) eventsSince(n int) []event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]event(nil), h.events[n:]...)
}
