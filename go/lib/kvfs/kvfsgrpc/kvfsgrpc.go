// Package kvfsgrpc serves a business tier's file stores over gRPC: the
// w17.common.files.W17Files service (proto/common/files/files.proto), mapped
// one to one onto the kvfs calls a Go business tier makes in-process.
//
//	Go business tier                              W17Files
//	kvfs.Put(ctx, clients.<Conn>Store(), gen.<Msg><Field>Placement, body)   Put
//	clients.<Conn>Store().Open(ctx, handle)                                 Open
//	clients.<Conn>Store().Stat(ctx, handle)                                 Stat
//	clients.<Conn>Store().Delete(ctx, handle)   (a writable store only)     Delete
//
// The handle a Put returns is what [kvfs.Put] returned, byte for byte, so a
// file stored through this server is laid out, named and read back exactly
// as one a Go tier stored: the same function made it.
//
// The server reads no environment and no manifest; it is handed the opened
// stores and the placements (the business companion, go/service/bizcompanion,
// builds them from the manifest). Every refusal is a status carrying a
// google.rpc.ErrorInfo with domain [Domain]; no message ever contains a store
// root, a bucket URL or a credential — a driver's own error text is reported
// to the process's error sink, never to the caller.
package kvfsgrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	smithyhttp "github.com/aws/smithy-go/transport/http"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	filespb "github.com/wandering-compiler/sdk/go/pb/common/files"
)

// Domain is the ErrorInfo domain of every refusal this server answers with.
const Domain = "w17.files"

// The ErrorInfo reasons, one per row of the error table in files.proto.
const (
	ReasonNotFound          = "FILE_NOT_FOUND"
	ReasonTooLarge          = "FILE_TOO_LARGE"
	ReasonUnknownField      = "UNKNOWN_FIELD"
	ReasonUnknownConnection = "UNKNOWN_CONNECTION"
	ReasonReadOnly          = "READ_ONLY_CONNECTION"
	ReasonBadRequest        = "BAD_REQUEST"
	ReasonStoreError        = "STORE_ERROR"
)

const (
	// MaxChunkBytes is the largest Put chunk the server accepts.
	MaxChunkBytes = 1 << 20
	// ChunkBytes is the size of the chunks Open sends (the last one may be
	// shorter); the SDKs send Put chunks of the same size.
	ChunkBytes = 64 << 10
)

// Conn is one upload connection's store.
type Conn struct {
	// Driver is the connection's opened store.
	Driver kvfs.Driver
	// Writable is true for a connection the business tier writes (its
	// fields declare `upload.writer: BUSINESS`). A connection the gateway
	// writes is read-only here: a Go tier holds only a kvfs.Reader for it,
	// which has no Delete.
	Writable bool
}

// Placement is a `writer: BUSINESS` table field's placement: the
// connection it stores on and its `(w17.field).upload` knobs, as the code
// generator bakes them into a Go tier's `gen.<Msg><Field>Placement`.
type Placement struct {
	Connection string
	kvfs.Placement
}

// NewServer serves conns (keyed by the schema's connection name) and
// placements (keyed by the field's full proto name,
// "app.example.NoteExport.file"). The maps are not copied; the caller must
// not change them afterwards.
func NewServer(conns map[string]Conn, placements map[string]Placement) filespb.W17FilesServer {
	return &server{conns: conns, placements: placements}
}

type server struct {
	filespb.UnimplementedW17FilesServer
	conns      map[string]Conn
	placements map[string]Placement
}

// Put stores the body that follows the header as a new object of the
// header's field: kvfs.Put with the field's placement, streaming the chunks
// straight into its staging file.
//
// Only the end marker completes the body. A stream that closes without it
// is not a body that happens to be shorter: a client whose body failed, or
// one cancelling the call (grpc-js half-closes before it resets the
// stream), reaches the server as exactly that close. kvfs.Put is then
// handed a read error, removes its staging file and stores nothing.
//
// A stalled body holds the call, and its staging file, until the client's
// deadline or cancellation — as kvfs.Put on a stalled reader does in a Go
// tier; the server sets no idle timeout of its own.
func (s *server) Put(stream filespb.W17Files_PutServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return badRequest("an empty Put: the first message must be the header")
		}
		return recvError(ctx, err)
	}
	hdr := first.GetHeader()
	if hdr == nil {
		return badRequest("the first message of a Put must be the header")
	}
	p, ok := s.placements[hdr.GetField()]
	if !ok {
		return fail(codes.InvalidArgument, ReasonUnknownField,
			fmt.Sprintf("%q is not a field this business tier writes files for", hdr.GetField()),
			map[string]string{"field": hdr.GetField()})
	}
	conn, ok := s.conns[p.Connection]
	if !ok {
		// A placement on a connection the server was not given: the
		// caller's configuration, not the request, is wrong.
		return unknownConnection(p.Connection)
	}
	if !conn.Writable {
		return readOnly(p.Connection)
	}
	body := &putBody{stream: stream}
	handle, err := kvfs.Put(ctx, conn.Driver, p.Placement, body)
	if err == nil {
		return stream.SendAndClose(&filespb.PutResponse{Handle: handle})
	}
	var bad badRequestError
	switch {
	case ctx.Err() != nil:
		// The call ended (cancelled, out of time) while the body arrived
		// or before the commit: that, not what it left behind, is the
		// answer.
		return status.FromContextError(ctx.Err()).Err()
	case errors.As(err, &bad):
		return badRequest(bad.msg)
	case body.recvErr != nil:
		return recvError(ctx, body.recvErr)
	case errors.Is(err, kvfs.ErrTooLarge):
		limit := strconv.FormatUint(p.MaxSizeBytes, 10)
		return fail(codes.InvalidArgument, ReasonTooLarge,
			fmt.Sprintf("the file is larger than %s allows (%s bytes)", hdr.GetField(), limit),
			map[string]string{"field": hdr.GetField(), "limit_bytes": limit})
	}
	return storeError(ctx, "put", p.Connection, err)
}

// putBody is the Put stream's body as an io.Reader: it pulls the next chunk
// from the stream when the last one is used up, so the body flows from the
// caller into kvfs.Put's staging file without a goroutine or a buffer of
// its own, and kvfs.Put stops pulling the moment a body is too large.
//
// It reports io.EOF only for a stream that sent its end marker, with the
// size of the chunks before it, and then closed; any other close is a read
// error, so kvfs.Put commits nothing.
type putBody struct {
	stream  filespb.W17Files_PutServer
	buf     []byte
	size    int64 // the bytes of the chunks received so far
	ended   bool  // the end marker arrived
	err     error // the error every later Read returns
	recvErr error // a transport error from Recv (cancellation, a broken stream)
}

func (b *putBody) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		msg, err := b.stream.Recv()
		switch {
		case errors.Is(err, io.EOF) && b.ended:
			b.err = io.EOF
		case errors.Is(err, io.EOF):
			b.err = badRequestError{"the Put stream closed before its end marker: the body is incomplete, and nothing is stored"}
		case err != nil:
			b.recvErr, b.err = err, err
		case b.ended:
			b.err = badRequestError{"a Put's end marker is its last message; another one followed it"}
		case msg.GetHeader() != nil:
			b.err = badRequestError{"a Put carries one header, first; this one repeats it"}
		case msg.GetEnd() != nil:
			if got := msg.GetEnd().GetSize(); got != b.size {
				b.err = badRequestError{fmt.Sprintf("the Put's end marker says %d bytes; its chunks carried %d", got, b.size)}
				break
			}
			b.ended = true
		default:
			c, ok := msg.GetPart().(*filespb.PutRequest_Chunk)
			if !ok {
				b.err = badRequestError{"a Put message after the header must be a chunk or the end marker"}
				break
			}
			if len(c.Chunk) > MaxChunkBytes {
				b.err = badRequestError{fmt.Sprintf("a Put chunk is at most %d bytes; this one has %d", MaxChunkBytes, len(c.Chunk))}
				break
			}
			b.size += int64(len(c.Chunk))
			b.buf = c.Chunk
		}
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

type badRequestError struct{ msg string }

func (e badRequestError) Error() string { return e.msg }

// Open streams the stored object in chunks of at most [ChunkBytes].
func (s *server) Open(req *filespb.OpenRequest, stream filespb.W17Files_OpenServer) error {
	ctx := stream.Context()
	conn, err := s.conn(req.GetConnection(), req.GetHandle())
	if err != nil {
		return err
	}
	rc, err := conn.Driver.Open(ctx, req.GetHandle())
	if err != nil {
		return driverError(ctx, "open", req.GetConnection(), err)
	}
	defer func() { _ = rc.Close() }()
	buf := make([]byte, ChunkBytes)
	for {
		n, rerr := io.ReadFull(rc, buf)
		if n > 0 {
			if err := stream.Send(&filespb.OpenResponse{Chunk: buf[:n]}); err != nil {
				return err
			}
		}
		switch {
		case rerr == nil:
			continue
		case errors.Is(rerr, io.EOF), errors.Is(rerr, io.ErrUnexpectedEOF):
			return nil
		}
		return storeError(ctx, "read", req.GetConnection(), rerr)
	}
}

// Stat reports the stored object's size and modification time.
func (s *server) Stat(ctx context.Context, req *filespb.StatRequest) (*filespb.StatResponse, error) {
	conn, err := s.conn(req.GetConnection(), req.GetHandle())
	if err != nil {
		return nil, err
	}
	info, err := conn.Driver.Stat(ctx, req.GetHandle())
	if err != nil {
		return nil, driverError(ctx, "stat", req.GetConnection(), err)
	}
	return &filespb.StatResponse{Size: info.Size, ModTime: timestamppb.New(info.ModTime)}, nil
}

// Delete removes a stored object of a connection the business tier writes.
// A missing object is not an error (kvfs.Driver.Delete is idempotent).
func (s *server) Delete(ctx context.Context, req *filespb.DeleteRequest) (*filespb.DeleteResponse, error) {
	conn, err := s.conn(req.GetConnection(), req.GetHandle())
	if err != nil {
		return nil, err
	}
	if !conn.Writable {
		return nil, readOnly(req.GetConnection())
	}
	if err := conn.Driver.Delete(ctx, req.GetHandle()); err != nil {
		return nil, storeError(ctx, "delete", req.GetConnection(), err)
	}
	return &filespb.DeleteResponse{}, nil
}

// conn resolves a request's connection and checks its handle.
func (s *server) conn(name, handle string) (Conn, error) {
	conn, ok := s.conns[name]
	if !ok {
		return Conn{}, unknownConnection(name)
	}
	if handle == "" {
		return Conn{}, badRequest("the handle is empty")
	}
	// A handle is what Put returned: hex segments. A dot-segment (`.`,
	// `..`, the local driver's `.w17-staging`) names no stored file, and
	// would reach another request's in-flight upload.
	for _, seg := range strings.Split(handle, "/") {
		if strings.HasPrefix(seg, ".") {
			return Conn{}, badRequest("the handle is not one Put returned")
		}
	}
	return conn, nil
}

// driverError maps an Open or Stat failure: a missing object is
// FILE_NOT_FOUND, anything else a store error.
func driverError(ctx context.Context, op, connection string, err error) error {
	if errors.Is(err, kvfs.ErrNotFound) {
		return fail(codes.NotFound, ReasonNotFound, "no stored file has this handle on "+connection,
			map[string]string{"connection": connection})
	}
	return storeError(ctx, op, connection, err)
}

// storeError is a driver failure: INTERNAL, or UNAVAILABLE when the driver
// could not reach its object store. The driver's text (which can name a
// bucket or a path) goes to the error sink, never into the status; a call
// that ended because its own context did is answered with that.
func storeError(ctx context.Context, op, connection string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr).Err()
	}
	observx.ReportError(ctx, fmt.Errorf("kvfsgrpc: %s on connection %s: %w", op, connection, err))
	code := codes.Internal
	if transportFailure(err) {
		code = codes.Unavailable
	}
	return fail(code, ReasonStoreError, "the file store of "+connection+" failed to "+op,
		map[string]string{"connection": connection})
}

// transportFailure reports an error that never got an answer from a remote
// store: the S3 client's send failure, or a network error under it.
func transportFailure(err error) bool {
	var send *smithyhttp.RequestSendError
	var netErr net.Error
	return errors.As(err, &send) || errors.As(err, &netErr)
}

// recvError is a broken Put stream: the caller's cancellation or deadline as
// such, any other transport error as it came.
func recvError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr).Err()
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Unknown, err.Error())
}

func badRequest(msg string) error {
	return fail(codes.InvalidArgument, ReasonBadRequest, msg, nil)
}

func unknownConnection(name string) error {
	return fail(codes.InvalidArgument, ReasonUnknownConnection,
		fmt.Sprintf("%q is not an upload connection of this business tier", name),
		map[string]string{"connection": name})
}

func readOnly(name string) error {
	return fail(codes.FailedPrecondition, ReasonReadOnly,
		"the gateway writes "+name+"; this business tier only reads it",
		map[string]string{"connection": name})
}

// fail is a status with the files ErrorInfo.
func fail(code codes.Code, reason, msg string, meta map[string]string) error {
	st := status.New(code, Domain+": "+msg)
	if withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Domain: Domain, Reason: reason, Metadata: meta}); err == nil {
		st = withInfo
	}
	return st.Err()
}
