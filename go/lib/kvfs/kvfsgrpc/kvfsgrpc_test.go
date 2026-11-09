package kvfsgrpc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/kvfsgrpc"
	localfs "github.com/wandering-compiler/sdk/go/lib/kvfs/local_fs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	filespb "github.com/wandering-compiler/sdk/go/pb/common/files"
)

const (
	exportField = "app.example.NoteExport.file"
	mediaField  = "app.example.Photo.image" // a placement on the read-only connection
)

var exportPlacement = kvfs.Placement{BucketPath: "/note-exports", ExpectedObjects: 100000, MaxPerBucket: 100, MaxSizeBytes: 300 << 10}

type fixture struct {
	client  filespb.W17FilesClient
	root    string // the exports connection's directory
	media   *memory.Driver
	handled chan string // the full method of every handler that returned
}

// serve runs the server over an in-memory listener: a real gRPC round trip,
// streams and status details included.
func serve(t *testing.T, conns map[string]kvfsgrpc.Conn, placements map[string]kvfsgrpc.Placement) (filespb.W17FilesClient, chan string) {
	t.Helper()
	handled := make(chan string, 64)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			defer func() { handled <- info.FullMethod }()
			return h(srv, ss)
		}),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			defer func() { handled <- info.FullMethod }()
			return h(ctx, req)
		}),
	)
	filespb.RegisterW17FilesServer(srv, kvfsgrpc.NewServer(conns, placements))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return filespb.NewW17FilesClient(cc), handled
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	media := memory.New()
	client, handled := serve(t,
		map[string]kvfsgrpc.Conn{
			"app-exports": {Driver: localfs.New(root), Writable: true},
			"app-media":   {Driver: media, Writable: false},
		},
		map[string]kvfsgrpc.Placement{
			exportField: {Connection: "app-exports", Placement: exportPlacement},
			mediaField:  {Connection: "app-media", Placement: kvfs.Placement{BucketPath: "/photos"}},
		})
	return &fixture{client: client, root: root, media: media, handled: handled}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// put sends the header and then body in chunks of chunk bytes.
func put(t *testing.T, c filespb.W17FilesClient, field string, body []byte, chunk int) (string, error) {
	t.Helper()
	st, err := c.Put(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_Header{Header: &filespb.PutHeader{Field: field}}}); err != nil {
		t.Fatal(err)
	}
	for len(body) > 0 {
		n := min(chunk, len(body))
		if err := st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_Chunk{Chunk: body[:n]}}); err != nil {
			break // the server answered early; CloseAndRecv says why
		}
		body = body[n:]
	}
	resp, err := st.CloseAndRecv()
	if err != nil {
		return "", err
	}
	return resp.GetHandle(), nil
}

func read(t *testing.T, c filespb.W17FilesClient, conn, handle string) ([]byte, int, error) {
	t.Helper()
	st, err := c.Open(ctx(t), &filespb.OpenRequest{Connection: conn, Handle: handle})
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	chunks := 0
	for {
		msg, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return out, chunks, nil
		}
		if err != nil {
			return nil, chunks, err
		}
		if len(msg.GetChunk()) > kvfsgrpc.ChunkBytes {
			t.Fatalf("Open sent a chunk of %d bytes", len(msg.GetChunk()))
		}
		chunks++
		out = append(out, msg.GetChunk()...)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// wantRefusal asserts err is a status with code and the files ErrorInfo.
func wantRefusal(t *testing.T, err error, code codes.Code, reason string, meta map[string]string) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != code {
		t.Fatalf("got %v, want %s", err, code)
	}
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			info = i
		}
	}
	if info == nil {
		t.Fatalf("%v carries no ErrorInfo", err)
	}
	if info.GetDomain() != kvfsgrpc.Domain || info.GetReason() != reason {
		t.Fatalf("ErrorInfo %s/%s, want %s/%s", info.GetDomain(), info.GetReason(), kvfsgrpc.Domain, reason)
	}
	if len(info.GetMetadata()) != len(meta) {
		t.Fatalf("metadata %v, want %v", info.GetMetadata(), meta)
	}
	for k, v := range meta {
		if info.GetMetadata()[k] != v {
			t.Fatalf("metadata %v, want %v", info.GetMetadata(), meta)
		}
	}
}

var handleShape = regexp.MustCompile(`^note-exports/([0-9a-f]{2}/)+([0-9a-f]{64})-[0-9a-f]{32}$`)

func TestRoundTrip(t *testing.T) {
	f := newFixture(t)
	body := randomBytes(t, 200<<10)
	handle, err := put(t, f.client, exportField, body, kvfsgrpc.ChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	// The handle is kvfs.Put's: BuildKey(bucket_path, <sha256>-<nonce>, …).
	m := handleShape.FindStringSubmatch(handle)
	if m == nil {
		t.Fatalf("handle %q is not <bucket>/<sub-buckets>/<sha256>-<nonce>", handle)
	}
	sum := sha256.Sum256(body)
	if m[2] != hex.EncodeToString(sum[:]) {
		t.Fatalf("handle hash %s, body sha256 %x", m[2], sum)
	}
	objectKey := handle[strings.LastIndex(handle, "/")+1:]
	if want := kvfs.BuildKey(exportPlacement.BucketPath, objectKey, exportPlacement.ExpectedObjects, exportPlacement.MaxPerBucket); handle != want {
		t.Fatalf("handle %q, BuildKey says %q", handle, want)
	}
	onDisk, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(handle)))
	if err != nil || !bytes.Equal(onDisk, body) {
		t.Fatalf("the file under the root: %v (equal=%v)", err, bytes.Equal(onDisk, body))
	}

	st, err := f.client.Stat(ctx(t), &filespb.StatRequest{Connection: "app-exports", Handle: handle})
	if err != nil {
		t.Fatal(err)
	}
	if st.GetSize() != int64(len(body)) || st.GetModTime().AsTime().IsZero() {
		t.Fatalf("stat %v", st)
	}
	got, chunks, err := read(t, f.client, "app-exports", handle)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("open: %v (equal=%v)", err, bytes.Equal(got, body))
	}
	if chunks != 4 { // 200 KiB in 64 KiB chunks
		t.Fatalf("Open sent %d chunks, want 4", chunks)
	}

	if _, err := f.client.Delete(ctx(t), &filespb.DeleteRequest{Connection: "app-exports", Handle: handle}); err != nil {
		t.Fatal(err)
	}
	_, _, err = read(t, f.client, "app-exports", handle)
	wantRefusal(t, err, codes.NotFound, kvfsgrpc.ReasonNotFound, map[string]string{"connection": "app-exports"})
	_, err = f.client.Stat(ctx(t), &filespb.StatRequest{Connection: "app-exports", Handle: handle})
	wantRefusal(t, err, codes.NotFound, kvfsgrpc.ReasonNotFound, map[string]string{"connection": "app-exports"})
	// Delete is idempotent.
	if _, err := f.client.Delete(ctx(t), &filespb.DeleteRequest{Connection: "app-exports", Handle: handle}); err != nil {
		t.Fatalf("a second delete: %v", err)
	}
}

func TestPutChunkingDoesNotChangeTheBody(t *testing.T) {
	f := newFixture(t)
	body := randomBytes(t, 100<<10)
	for _, chunk := range []int{1 << 10, 7777, kvfsgrpc.MaxChunkBytes} {
		handle, err := put(t, f.client, exportField, body, chunk)
		if err != nil {
			t.Fatalf("chunk %d: %v", chunk, err)
		}
		got, _, err := read(t, f.client, "app-exports", handle)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("chunk %d: %v (equal=%v)", chunk, err, bytes.Equal(got, body))
		}
	}
}

func TestPutEmptyBodyStoresAnEmptyObject(t *testing.T) {
	f := newFixture(t)
	handle, err := put(t, f.client, exportField, nil, kvfsgrpc.ChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.client.Stat(ctx(t), &filespb.StatRequest{Connection: "app-exports", Handle: handle})
	if err != nil || st.GetSize() != 0 {
		t.Fatalf("stat %v, %v", st, err)
	}
	sum := sha256.Sum256(nil)
	if !strings.Contains(handle, hex.EncodeToString(sum[:])+"-") {
		t.Fatalf("handle %q does not carry the empty body's hash", handle)
	}
}

func TestPutTooLarge(t *testing.T) {
	f := newFixture(t)
	_, err := put(t, f.client, exportField, randomBytes(t, 300<<10+1), kvfsgrpc.ChunkBytes)
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonTooLarge,
		map[string]string{"field": exportField, "limit_bytes": "307200"})
	// Exactly the cap is fine.
	if _, err := put(t, f.client, exportField, randomBytes(t, 300<<10), kvfsgrpc.ChunkBytes); err != nil {
		t.Fatal(err)
	}
}

func TestPutNoCap(t *testing.T) {
	mem := memory.New()
	client, _ := serve(t, map[string]kvfsgrpc.Conn{"c": {Driver: mem, Writable: true}},
		map[string]kvfsgrpc.Placement{"a.B.f": {Connection: "c", Placement: kvfs.Placement{BucketPath: "b"}}})
	body := randomBytes(t, 2<<20)
	handle, err := put(t, client, "a.B.f", body, kvfsgrpc.ChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(handle, "b/") || !mem.Has(handle) {
		t.Fatalf("handle %q (stored=%v)", handle, mem.Has(handle))
	}
}

func TestPutRefusals(t *testing.T) {
	f := newFixture(t)
	_, err := put(t, f.client, "app.example.Nope.file", []byte("x"), 1)
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonUnknownField, map[string]string{"field": "app.example.Nope.file"})
	_, err = put(t, f.client, mediaField, []byte("x"), 1)
	wantRefusal(t, err, codes.FailedPrecondition, kvfsgrpc.ReasonReadOnly, map[string]string{"connection": "app-media"})

	send := func(msgs ...*filespb.PutRequest) error {
		st, err := f.client.Put(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if err := st.Send(m); err != nil {
				break
			}
		}
		_, err = st.CloseAndRecv()
		return err
	}
	header := &filespb.PutRequest{Part: &filespb.PutRequest_Header{Header: &filespb.PutHeader{Field: exportField}}}
	chunk := func(n int) *filespb.PutRequest {
		return &filespb.PutRequest{Part: &filespb.PutRequest_Chunk{Chunk: make([]byte, n)}}
	}
	for name, msgs := range map[string][]*filespb.PutRequest{
		"no message":        nil,
		"chunk first":       {chunk(3)},
		"empty first":       {{}},
		"repeated header":   {header, chunk(3), header},
		"empty after":       {header, {}},
		"chunk above 1 MiB": {header, chunk(kvfsgrpc.MaxChunkBytes + 1)},
	} {
		t.Run(name, func(t *testing.T) {
			wantRefusal(t, send(msgs...), codes.InvalidArgument, kvfsgrpc.ReasonBadRequest, nil)
		})
	}
}

func TestPutPlacementOnAMissingConnection(t *testing.T) {
	client, _ := serve(t, map[string]kvfsgrpc.Conn{},
		map[string]kvfsgrpc.Placement{"a.B.f": {Connection: "gone", Placement: kvfs.Placement{BucketPath: "b"}}})
	_, err := put(t, client, "a.B.f", []byte("x"), 1)
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonUnknownConnection, map[string]string{"connection": "gone"})
}

func TestCancelledPutStoresNothing(t *testing.T) {
	mem := memory.New()
	client, handled := serve(t, map[string]kvfsgrpc.Conn{"c": {Driver: mem, Writable: true}},
		map[string]kvfsgrpc.Placement{"a.B.f": {Connection: "c", Placement: kvfs.Placement{BucketPath: "b"}}})
	c, cancel := context.WithCancel(context.Background())
	st, err := client.Put(c)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_Header{Header: &filespb.PutHeader{Field: "a.B.f"}}})
	_ = st.Send(&filespb.PutRequest{Part: &filespb.PutRequest_Chunk{Chunk: []byte("partial")}})
	cancel()
	// RecvMsg, not CloseAndRecv: a half-close racing the cancellation would
	// tell the server the body is complete, and it would rightly store it.
	if err := st.RecvMsg(&filespb.PutResponse{}); status.Code(err) != codes.Canceled {
		t.Fatalf("got %v, want Canceled", err)
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the Put handler did not return")
	}
	if keys := mem.Keys(); len(keys) != 0 {
		t.Fatalf("a cancelled Put stored %v", keys)
	}
}

func TestOpenStatDeleteRefusals(t *testing.T) {
	f := newFixture(t)
	f.media.PutBytes("photos/p.png", []byte("png"))

	_, err := f.client.Stat(ctx(t), &filespb.StatRequest{Connection: "nope", Handle: "x"})
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonUnknownConnection, map[string]string{"connection": "nope"})
	_, _, err = read(t, f.client, "nope", "x")
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonUnknownConnection, map[string]string{"connection": "nope"})
	_, err = f.client.Delete(ctx(t), &filespb.DeleteRequest{Connection: "nope", Handle: "x"})
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonUnknownConnection, map[string]string{"connection": "nope"})

	_, err = f.client.Stat(ctx(t), &filespb.StatRequest{Connection: "app-exports"})
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonBadRequest, nil)
	_, _, err = read(t, f.client, "app-exports", "")
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonBadRequest, nil)
	_, err = f.client.Delete(ctx(t), &filespb.DeleteRequest{Connection: "app-exports"})
	wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonBadRequest, nil)

	// A read-only connection reads, and refuses Delete.
	got, _, err := read(t, f.client, "app-media", "photos/p.png")
	if err != nil || string(got) != "png" {
		t.Fatalf("read-only open: %q, %v", got, err)
	}
	_, err = f.client.Delete(ctx(t), &filespb.DeleteRequest{Connection: "app-media", Handle: "photos/p.png"})
	wantRefusal(t, err, codes.FailedPrecondition, kvfsgrpc.ReasonReadOnly, map[string]string{"connection": "app-media"})
	if !f.media.Has("photos/p.png") {
		t.Fatal("a refused Delete removed the object")
	}
}

// broken is a driver whose every call fails with err.
type broken struct{ err error }

func (b broken) Open(context.Context, string) (io.ReadCloser, error) { return nil, b.err }
func (b broken) Stat(context.Context, string) (kvfs.Info, error)     { return kvfs.Info{}, b.err }
func (b broken) OpenSeekable(context.Context, string) (io.ReadSeekCloser, error) {
	return nil, b.err
}
func (b broken) PutFromTempFile(context.Context, string, string) (string, error) { return "", b.err }
func (b broken) Delete(context.Context, string) error                            { return b.err }

func TestStoreErrorsDoNotLeakTheDriverText(t *testing.T) {
	secretText := "s3://AKIA:secret@bucket/root"
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{
		{errors.New("kvfs s3: put in bucket " + secretText), codes.Internal},
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New(secretText)}, codes.Unavailable},
	} {
		client, _ := serve(t, map[string]kvfsgrpc.Conn{"c": {Driver: broken{tc.err}, Writable: true}},
			map[string]kvfsgrpc.Placement{"a.B.f": {Connection: "c", Placement: kvfs.Placement{BucketPath: "b"}}})
		meta := map[string]string{"connection": "c"}
		_, err := put(t, client, "a.B.f", []byte("x"), 1)
		wantRefusal(t, err, tc.code, kvfsgrpc.ReasonStoreError, meta)
		errs := []error{err}
		_, err = client.Stat(ctx(t), &filespb.StatRequest{Connection: "c", Handle: "h"})
		wantRefusal(t, err, tc.code, kvfsgrpc.ReasonStoreError, meta)
		errs = append(errs, err)
		_, _, err = read(t, client, "c", "h")
		wantRefusal(t, err, tc.code, kvfsgrpc.ReasonStoreError, meta)
		errs = append(errs, err)
		_, err = client.Delete(ctx(t), &filespb.DeleteRequest{Connection: "c", Handle: "h"})
		wantRefusal(t, err, tc.code, kvfsgrpc.ReasonStoreError, meta)
		errs = append(errs, err)
		for _, e := range errs {
			if strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "bucket") {
				t.Fatalf("the status repeats the driver's text: %v", e)
			}
		}
	}
}

// failingReader fails after its first bytes: an object whose read breaks
// mid-stream.
type failingReader struct{ sent bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "abc"), nil
	}
	return 0, errors.New("disk on fire at /var/lib/w17/uploads")
}
func (r *failingReader) Close() error { return nil }

type midStream struct{ broken }

func (midStream) Open(context.Context, string) (io.ReadCloser, error) {
	return &failingReader{}, nil
}

func TestOpenReadFailureMidStream(t *testing.T) {
	client, _ := serve(t, map[string]kvfsgrpc.Conn{"c": {Driver: midStream{}}}, nil)
	_, _, err := read(t, client, "c", "h")
	wantRefusal(t, err, codes.Internal, kvfsgrpc.ReasonStoreError, map[string]string{"connection": "c"})
	if strings.Contains(err.Error(), "/var/lib") {
		t.Fatalf("the status repeats the driver's text: %v", err)
	}
}
