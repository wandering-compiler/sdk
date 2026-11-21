package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
	"github.com/wandering-compiler/sdk/go/lib/paging"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// PagedTool describes a tool whose method is cursor-paged: its storage query
// runs a keyset WHERE and a LIMIT, so without a cursor and a limit of its own
// the tool would answer the first page of every list as if it were the whole
// of it. The generated handler declares one per paged tool and calls
// [Server.CallPaged] in place of [Server.CallUnary].
//
// The tool takes the paging the REST endpoint of the same method takes, as
// two arguments beside the request's own fields: CursorParam (a page's
// `next_cursor` or `previous_cursor`, passed back) and LimitParam (the page
// size, clamped to MaxLimit). The cursors are minted and checked exactly as
// the REST gateway mints and checks them, from the same request, so a page
// reached by cursor is the page REST would have answered.
type PagedTool struct {
	CursorParam string
	LimitParam  string

	DefaultLimit uint32
	MaxLimit     uint32

	// SchemaVersion is the request schema the cursors are minted under; a
	// cursor minted under another one (a different build) is refused, and the
	// caller starts again from page one.
	SchemaVersion uint32

	// PagingField and ItemsField are the proto names of the response's
	// w17.Paging envelope and of its repeated item list.
	PagingField protoreflect.Name
	ItemsField  protoreflect.Name

	// FromPosition marks a method with no ORDER BY of its own (a business
	// method relaying a storage page): the boundaries come back as the
	// response headers the backend reports, not from the items.
	FromPosition bool
}

// PageBounds reads the sort-key values of the response's i-th item in its
// query's ORDER BY, the keyset a cursor resumes from. The generated invoke
// returns it over its typed response, so each value is encoded as the
// storage side compares it. Nil when the tool is FromPosition.
type PageBounds func(i int) []*w17pb.KeysetValue

// PagedInvoke is a paged tool's backend call: the typed client call, given
// the call options (the header capture of a FromPosition tool), answering
// the response and the reader of its items' boundaries.
type PagedInvoke func(ctx context.Context, reqMsg proto.Message, opts ...grpc.CallOption) (proto.Message, PageBounds, error)

// SetCursorKey sets the per-deployment HMAC key paged tools sign their
// cursors with (paging.CursorKeyFromEnv, as the REST gateway resolves it).
// Without one a paged tool still answers, under the development key.
func (s *Server) SetCursorKey(key []byte) { s.cursorKey = key }

// CallPaged is CallUnary for a cursor-paged tool: everything CallUnary does
// (permission gate, auth metadata, argument decode, the request event, the
// result encoding), plus the paging the REST gateway does around the same
// call — the cursor restores the request it was minted from, the limit and
// the keyset boundaries reach storage as metadata, and the response's paging
// envelope is filled with the page size, the total and the cursors onward.
func (s *Server) CallPaged(
	ctx context.Context,
	req mcp.CallToolRequest,
	reqMsg proto.Message,
	p *PagedTool,
	invoke PagedInvoke,
) (*mcp.CallToolResult, error) {
	return s.CallUnary(ctx, req, reqMsg, func(ctx context.Context, reqMsg proto.Message) (proto.Message, error) {
		return s.callPaged(ctx, req, reqMsg, p, invoke)
	})
}

func (s *Server) callPaged(
	ctx context.Context,
	req mcp.CallToolRequest,
	reqMsg proto.Message,
	p *PagedTool,
	invoke PagedInvoke,
) (proto.Message, error) {
	cursor, limitReq, err := pagingArgs(ctx, req, p)
	if err != nil {
		return nil, err
	}

	// The cursor carries the minting request whole, so it is signed for the
	// caller it was minted for: presented by anyone else it fails as
	// malformed. Authorization and scopes are still applied on every page.
	key := paging.CallerKey(s.cursorKey, callerParts(ctx)...)

	var (
		boundaries  []*w17pb.KeysetValue
		dir         w17pb.Direction
		cachedTotal uint64
	)
	if cursor != "" {
		proto.Reset(reqMsg)
		bs, total, d, sortBy, cerr := paging.DecodeCursor(cursor, reqMsg, p.SchemaVersion, key)
		if cerr != nil {
			code := "INVALID_VALUE"
			if errors.Is(cerr, paging.ErrCursorExpired) {
				code = "CURSOR_EXPIRED"
			}
			return nil, badArgument(ctx, p.CursorParam, code, cerr)
		}
		// One keyset variant per tool (its DQL ORDER BY), so selector 0; a
		// cursor minted under another sort would seek the wrong columns.
		if sortBy != 0 {
			return nil, badArgument(ctx, p.CursorParam, "INVALID_VALUE", fmt.Errorf("cursor minted under sort selector %d", sortBy))
		}
		boundaries, dir, cachedTotal = bs, d, total
	}

	if len(boundaries) > 0 {
		if v, err := paging.EncodeBoundariesMD(boundaries, dir); err == nil && v != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, paging.BoundariesMDKey, v)
		}
	}
	limit := paging.ClampLimit(limitReq, p.DefaultLimit, p.MaxLimit)
	if v := paging.EncodeLimitMD(limit); v != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, paging.LimitMDKey, v)
	}
	if cachedTotal != 0 {
		if v := paging.EncodeTotalMD(cachedTotal); v != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, paging.TotalMDKey, v)
		}
	}

	var (
		hdr  metadata.MD
		opts []grpc.CallOption
	)
	if p.FromPosition {
		opts = append(opts, grpc.Header(&hdr))
	}
	resp, bounds, err := invoke(ctx, reqMsg, opts...)
	if err != nil || resp == nil {
		return resp, err
	}
	fillPaging(resp, reqMsg, p, bounds, key, limit, boundaries, dir, hdr)
	return resp, nil
}

// pagingArgs reads the cursor and the limit from the tool's arguments. The
// request decode discards them (they are no field of the request), so they
// are read from the raw arguments here. A limit above MaxLimit is clamped and
// echoed; a limit that is no whole number is refused, as on REST.
func pagingArgs(ctx context.Context, req mcp.CallToolRequest, p *PagedTool) (string, uint32, error) {
	args, err := argumentMap(req.GetRawArguments())
	if err != nil {
		return "", 0, badArgument(ctx, p.CursorParam, "INVALID_VALUE", err)
	}
	var cursor string
	switch v := args[p.CursorParam].(type) {
	case nil:
	case string:
		cursor = v
	default:
		return "", 0, badArgument(ctx, p.CursorParam, "INVALID_VALUE", fmt.Errorf("cursor is %T, not a string", v))
	}
	var limit uint32
	switch v := args[p.LimitParam].(type) {
	case nil:
	case float64:
		if v < 0 || v != math.Trunc(v) || v > math.MaxUint32 {
			return "", 0, badArgument(ctx, p.LimitParam, "INVALID_VALUE", fmt.Errorf("limit %v is not a page size", v))
		}
		limit = uint32(v)
	case json.Number:
		n, err := strconv.ParseUint(string(v), 10, 32)
		if err != nil {
			return "", 0, badArgument(ctx, p.LimitParam, "INVALID_VALUE", err)
		}
		limit = uint32(n)
	case string:
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return "", 0, badArgument(ctx, p.LimitParam, "INVALID_VALUE", err)
		}
		limit = uint32(n)
	default:
		return "", 0, badArgument(ctx, p.LimitParam, "INVALID_VALUE", fmt.Errorf("limit is %T, not a number", v))
	}
	return cursor, limit, nil
}

// argumentMap is the tool's arguments as a map, in either shape they arrive
// in: decoded by the MCP library (a map), or captured raw by the transport
// so 64-bit integers stay exact (json.RawMessage, see exactArgumentHooks) —
// decoded here with UseNumber for the same reason.
func argumentMap(raw any) (map[string]any, error) {
	var data []byte
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return v, nil
	case json.RawMessage:
		data = v
	case []byte:
		data = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		data = b
	}
	if len(data) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("arguments are not an object: %w", err)
	}
	return m, nil
}

// badArgument is the refusal of one paging argument: INVALID_ARGUMENT with a
// detail naming it, the shape a validation refusal has, so the model sees
// which argument to drop. The cause reaches the operator only (CallUnary
// reports the returned status; publicError keeps the detail).
func badArgument(ctx context.Context, field, code string, cause error) error {
	st := status.New(codes.InvalidArgument, fmt.Sprintf("%s: %v", field, cause))
	if withDetail, err := st.WithDetails(&w17pb.ErrorDetail{
		Field:   field,
		Code:    code,
		Message: i18n.T(ctx, restgw.MsgidMalformedValue, nil),
	}); err == nil {
		st = withDetail
	}
	return st.Err()
}

// callerParts is the verified caller a cursor is bound to, read from the
// metadata the auth function attached for the backend: the user (field 2 of
// the auth response, the user_id every w17 auth response carries there), and
// the scopes and broadcast labels the request runs under, sorted so one
// caller always derives one key. Not the whole auth response: its
// permission_ids come in no fixed order, so the same caller would mint a
// different key per call.
func callerParts(ctx context.Context) []string {
	md, _ := metadata.FromOutgoingContext(ctx)
	parts := []string{"user:" + userIDFromMD(md)}
	var rest []string
	for k, vs := range md {
		if len(vs) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(k, "x-w17-scope-"):
			rest = append(rest, "scope:"+strings.TrimPrefix(k, "x-w17-scope-")+"="+vs[0])
		case strings.HasPrefix(k, "w17-label-"):
			rest = append(rest, "label:"+strings.TrimPrefix(k, "w17-label-")+"="+vs[0])
		}
	}
	sort.Strings(rest)
	return append(parts, rest...)
}

func userIDFromMD(md metadata.MD) string {
	v := md.Get("x-w17-user")
	if len(v) == 0 || v[0] == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(v[0])
	if err != nil {
		return ""
	}
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return ""
		}
		raw = raw[n:]
		if num == 2 && typ == protowire.BytesType {
			b, n := protowire.ConsumeBytes(raw)
			if n < 0 {
				return ""
			}
			return string(b)
		}
		n = protowire.ConsumeFieldValue(num, typ, raw)
		if n < 0 {
			return ""
		}
		raw = raw[n:]
	}
	return ""
}

// fillPaging writes the response's paging envelope as the REST gateway does:
// the applied limit, the total, and the cursors onward. A full forward page
// may have a successor, a short one is the last; a resumed page has a page
// behind it; an empty resumed page hands back the way it came.
func fillPaging(resp, req proto.Message, p *PagedTool, bounds PageBounds, key []byte, limit uint32, arrived []*w17pb.KeysetValue, dir w17pb.Direction, hdr metadata.MD) {
	rm := resp.ProtoReflect()
	desc := rm.Descriptor()
	pf := desc.Fields().ByName(p.PagingField)
	itf := desc.Fields().ByName(p.ItemsField)
	if pf == nil || itf == nil || !itf.IsList() || itf.Message() == nil {
		return
	}
	if pf.Message() == nil || pf.Message().FullName() != "w17.Paging" || pf.IsList() {
		return
	}
	pg := envelope{rm.Mutable(pf).Message()}
	pg.setUint("limit", uint64(limit))

	var pos paging.Position
	if p.FromPosition {
		pos, _ = paging.PositionFromHeader(hdr)
		if pg.uint("total") == 0 && pos.HasTotal {
			pg.setUint("total", pos.Total)
		}
	}

	mint := func(bs []*w17pb.KeysetValue, d w17pb.Direction) string {
		c, err := paging.EncodeCursor(req, bs, pg.uint("total"), d, p.SchemaVersion, 0, key)
		if err != nil {
			return ""
		}
		return c
	}

	items := rm.Get(itf).List()
	n := items.Len()
	if n == 0 {
		if len(arrived) == 0 {
			return
		}
		if dir == w17pb.Direction_DIRECTION_BACKWARD {
			pg.setString("next_cursor", mint(arrived, w17pb.Direction_DIRECTION_FORWARD))
		} else {
			pg.setString("previous_cursor", mint(arrived, w17pb.Direction_DIRECTION_BACKWARD))
		}
		return
	}
	// A backward page comes back in reverse order; flip it so the rows read
	// in one order whichever way the caller travels.
	if dir == w17pb.Direction_DIRECTION_BACKWARD {
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			a, b := items.Get(i), items.Get(j)
			items.Set(i, b)
			items.Set(j, a)
		}
	}
	full := uint64(n) >= uint64(limit)

	var next, prev []*w17pb.KeysetValue
	if p.FromPosition {
		next, prev = pos.Next, pos.Prev
	} else if bounds != nil {
		next, prev = bounds(n-1), bounds(0)
	}
	if len(next) > 0 && (full || dir == w17pb.Direction_DIRECTION_BACKWARD) {
		pg.setString("next_cursor", mint(next, w17pb.Direction_DIRECTION_FORWARD))
	}
	if len(prev) > 0 && len(arrived) > 0 && (full || dir == w17pb.Direction_DIRECTION_FORWARD) {
		pg.setString("previous_cursor", mint(prev, w17pb.Direction_DIRECTION_BACKWARD))
	}
}

// envelope is the response's w17.Paging, written by field name through
// reflection: the response type is the generated code's, so the envelope is
// reached as a message, not as the SDK's own *w17pb.Paging.
type envelope struct{ m protoreflect.Message }

func (e envelope) field(name protoreflect.Name) protoreflect.FieldDescriptor {
	return e.m.Descriptor().Fields().ByName(name)
}

func (e envelope) uint(name protoreflect.Name) uint64 {
	if fd := e.field(name); fd != nil {
		return e.m.Get(fd).Uint()
	}
	return 0
}

func (e envelope) setUint(name protoreflect.Name, v uint64) {
	fd := e.field(name)
	if fd == nil {
		return
	}
	if fd.Kind() == protoreflect.Uint32Kind {
		e.m.Set(fd, protoreflect.ValueOfUint32(uint32(v)))
		return
	}
	e.m.Set(fd, protoreflect.ValueOfUint64(v))
}

func (e envelope) setString(name protoreflect.Name, v string) {
	if fd := e.field(name); fd != nil && v != "" {
		e.m.Set(fd, protoreflect.ValueOfString(v))
	}
}
