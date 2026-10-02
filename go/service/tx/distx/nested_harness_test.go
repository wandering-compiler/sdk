package distx_test

// The harness for nested transactions and business composition.
//
// Everything here is REAL except the business methods themselves: a real
// distx server over a real txregistry, two SQLite databases on disk (":memory:"
// would give every pooled connection its own database), a storage service that
// adopts the transaction exactly as a generated storage handler does
// (txregistry.AdoptTx on the INCOMING `w17-tx-id`), and storage- and
// business-tier emits that park exactly as the generated emit wrappers do
// (txregistry.DeferUntilCommit — with the registry on the storage side, nil on
// the business side).
//
// Two transports, because the property that matters most differs between them:
//
//   - "grpc": business and storage are separate binaries (bufconn). Context
//     VALUES do not cross; only metadata does.
//   - "inproc": a composed binary (inprocgrpc). Context values DO cross, and
//     the hop moves the caller's outgoing metadata to the incoming side and
//     clears the outgoing one.
//
// A server interceptor stands in for the storage proxy: Begin answers with a
// `w17-conn-id`, and every later call is recorded with the tx and conn headers
// it actually carried, so affinity is asserted, not assumed.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	_ "modernc.org/sqlite"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

const (
	connMain  = "main"
	connAudit = "audit"
	replicaID = "replica-7"
)

var transports = []string{"grpc", "inproc"}

// call is one RPC as the server saw it.
type call struct {
	method  string
	txIDs   []string // every w17-tx-id value the call carried
	connIDs []string // every w17-conn-id value
	adopted bool     // storage only: ran inside a transaction
	value   string
}

type harness struct {
	t         *testing.T
	transport string
	reg       *txregistry.Memory
	dbs       map[string]*sql.DB

	client  distxpb.W17DistributedTransactionClient
	storage grpc.ClientConnInterface
	biz     grpc.ClientConnInterface // composed business hop (inproc only)

	mu      sync.Mutex
	calls   []call
	emitted []string

	// bizFn is what the composed business service runs when called.
	bizFn func(ctx context.Context, value string) error

	// ownTx makes the storage handler open its OWN transaction for each
	// write, the way a plugin handler does with distx.Begin.
	ownTx bool
}

func newHarness(t *testing.T, transport string) *harness {
	t.Helper()
	h := &harness{t: t, transport: transport, dbs: map[string]*sql.DB{}}
	dir := t.TempDir()
	for _, name := range []string{connMain, connAudit} {
		db, err := sql.Open("sqlite", filepath.Join(dir, name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
			t.Fatal(err)
		}
		h.dbs[name] = db
	}
	h.reg = txregistry.NewMemory(h.dbs)
	srv := distx.NewServer(h.reg)
	icpt := h.interceptor

	switch transport {
	case "grpc":
		gs := grpc.NewServer(grpc.UnaryInterceptor(icpt))
		distxpb.RegisterW17DistributedTransactionServer(gs, srv)
		gs.RegisterService(&storageDesc, h)
		lis := bufconn.Listen(1 << 20)
		go func() { _ = gs.Serve(lis) }()
		t.Cleanup(gs.Stop)
		cc, err := grpc.NewClient("passthrough://bufconn",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		h.client = distxpb.NewW17DistributedTransactionClient(cc)
		h.storage = cc
	case "inproc":
		c := inprocgrpc.New(inprocgrpc.WithUnaryInterceptor(icpt))
		distxpb.RegisterW17DistributedTransactionServer(c, srv)
		c.RegisterService(&storageDesc, h)
		h.client = distxpb.NewW17DistributedTransactionClient(c)
		h.storage = c
		biz := inprocgrpc.New()
		biz.RegisterService(&bizDesc, h)
		h.biz = biz
	default:
		t.Fatalf("unknown transport %q", transport)
	}
	return h
}

// interceptor stands in for the storage proxy and records every call.
func (h *harness) interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	c := call{method: info.FullMethod, txIDs: md.Get(txregistry.HeaderName), connIDs: md.Get(distx.ConnIDHeader)}
	if sv, ok := req.(*wrapperspb.StringValue); ok {
		c.value = sv.GetValue()
	}
	resp, err := handler(ctx, req)
	if strings.HasSuffix(info.FullMethod, "/Begin") && err == nil {
		_ = grpc.SetHeader(ctx, metadata.Pairs(distx.ConnIDHeader, replicaID))
	}
	h.mu.Lock()
	h.calls = append(h.calls, c)
	h.mu.Unlock()
	return resp, err
}

func (h *harness) emit(name string) {
	h.mu.Lock()
	h.emitted = append(h.emitted, name)
	h.mu.Unlock()
}

func (h *harness) emittedNow() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.emitted...)
}

func (h *harness) count(suffix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if strings.HasSuffix(c.method, suffix) {
			n++
		}
	}
	return n
}

func (h *harness) storageCalls() []call {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []call
	for _, c := range h.calls {
		if strings.HasSuffix(c.method, "/Insert") {
			out = append(out, c)
		}
	}
	return out
}

// rows reads what is COMMITTED on a connection, in insertion order.
func (h *harness) rows(conn string) []string {
	h.t.Helper()
	rs, err := h.dbs[conn].Query(`SELECT v FROM t ORDER BY rowid`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	var out []string
	for rs.Next() {
		var v string
		if err := rs.Scan(&v); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, v)
	}
	// A read that stopped early would otherwise look like "these are all the
	// rows" — and the assertions here are about which rows exist.
	if err := rs.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// write is a storage RPC: "insert value on conn", inside whatever transaction
// ctx carries.
func (h *harness) write(ctx context.Context, conn, value string) error {
	return h.storage.Invoke(ctx, "/test.Storage/Insert", wrapperspb.String(conn+":"+value), &emptypb.Empty{})
}

// bizEmit is what a generated BUSINESS emit wrapper does after its method
// returns: park on the transaction, or announce now. Registry nil — the
// business tier has none.
func (h *harness) bizEmit(ctx context.Context, name string) {
	if txregistry.DeferUntilCommit(ctx, nil, func() { h.emit("biz:" + name) }) == txregistry.EmitNow {
		h.emit("biz:" + name)
	}
}

// callBiz invokes the composed business service over its in-process conn —
// the hop that moves metadata from outgoing to incoming.
func (h *harness) callBiz(ctx context.Context, value string) error {
	return h.biz.Invoke(ctx, "/test.Biz/Do", wrapperspb.String(value), &emptypb.Empty{})
}

// insert is the storage handler: adopt like generated code, write, emit like
// the generated storage wrapper.
func (h *harness) insert(ctx context.Context, in *wrapperspb.StringValue) (*emptypb.Empty, error) {
	conn, value, _ := strings.Cut(in.GetValue(), ":")
	if h.ownTx && !strings.HasPrefix(value, "own-tx-inner:") {
		// A plugin-shaped handler: its own distx transaction, writing through
		// storage again — whatever transaction its caller is in.
		err := distx.Run(ctx, h.client, &distxpb.BeginRequest{ConnectionName: conn}, func(tctx context.Context) error {
			return h.write(tctx, conn, "own-tx-inner:"+value)
		})
		if err != nil {
			return nil, err
		}
		h.bizEmit(ctx, "handler:"+value)
		return &emptypb.Empty{}, nil
	}
	tx, adopted, release, err := txregistry.AdoptTx(ctx, h.reg, conn)
	if err != nil {
		return nil, err
	}
	defer release()
	if adopted {
		_, err = tx.ExecContext(ctx, `INSERT INTO t (v) VALUES (?)`, value)
	} else {
		_, err = h.dbs[conn].ExecContext(ctx, `INSERT INTO t (v) VALUES (?)`, value)
	}
	if err != nil {
		return nil, err
	}
	// The interceptor records the call after the handler returns; adoption is
	// only known in here, so it gets its own record.
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "adopted", adopted: adopted, value: in.GetValue()})
	h.mu.Unlock()
	if txregistry.DeferUntilCommit(ctx, h.reg, func() { h.emit("storage:" + value) }) == txregistry.EmitNow {
		h.emit("storage:" + value)
	}
	return &emptypb.Empty{}, nil
}

// adoptedFor reports whether the storage write of value ran inside a
// transaction.
func (h *harness) adoptedFor(conn, value string) (adopted, seen bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if c.method == "adopted" && c.value == conn+":"+value {
			return c.adopted, true
		}
	}
	return false, false
}

func (h *harness) do(ctx context.Context, in *wrapperspb.StringValue) (*emptypb.Empty, error) {
	if h.bizFn == nil {
		return nil, fmt.Errorf("no business function set")
	}
	return &emptypb.Empty{}, h.bizFn(ctx, in.GetValue())
}

func unaryHandler(name string, fn func(h *harness, ctx context.Context, in *wrapperspb.StringValue) (*emptypb.Empty, error)) grpc.MethodHandler {
	return func(srv any, ctx context.Context, dec func(any) error, icpt grpc.UnaryServerInterceptor) (any, error) {
		in := new(wrapperspb.StringValue)
		if err := dec(in); err != nil {
			return nil, err
		}
		h := srv.(*harness)
		if icpt == nil {
			return fn(h, ctx, in)
		}
		return icpt(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: name},
			func(ctx context.Context, req any) (any, error) { return fn(h, ctx, req.(*wrapperspb.StringValue)) })
	}
}

var storageDesc = grpc.ServiceDesc{
	ServiceName: "test.Storage",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{MethodName: "Insert", Handler: unaryHandler("/test.Storage/Insert",
		func(h *harness, ctx context.Context, in *wrapperspb.StringValue) (*emptypb.Empty, error) {
			return h.insert(ctx, in)
		})}},
}

var bizDesc = grpc.ServiceDesc{
	ServiceName: "test.Biz",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{MethodName: "Do", Handler: unaryHandler("/test.Biz/Do",
		func(h *harness, ctx context.Context, in *wrapperspb.StringValue) (*emptypb.Empty, error) {
			return h.do(ctx, in)
		})}},
}

func mainReq() *distxpb.BeginRequest { return &distxpb.BeginRequest{ConnectionName: connMain} }

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
