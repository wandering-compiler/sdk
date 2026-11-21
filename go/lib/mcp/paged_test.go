package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// pagedTestTypes builds the shape of a generated paged method: a request
// with one filter, a response with a repeated item list and a w17.Paging
// envelope. Dynamic, so the SDK needs no generated test proto of its own.
func pagedTestTypes(t *testing.T) (req, resp, item protoreflect.MessageDescriptor) {
	t.Helper()
	_ = w17pb.Paging{} // registers w17/paging.proto
	s := func(v string) *string { return &v }
	i := func(v int32) *int32 { return &v }
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	fd := &descriptorpb.FileDescriptorProto{
		Name:       s("pagedtest/paged.proto"),
		Package:    s("pagedtest"),
		Syntax:     s("proto3"),
		Dependency: []string{"w17/paging.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: s("Req"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: s("q"), JsonName: s("q"), Number: i(1), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
			}},
			{Name: s("Item"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: s("id"), JsonName: s("id"), Number: i(1), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
			}},
			{Name: s("Resp"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: s("items"), JsonName: s("items"), Number: i(1), Label: rep, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: s(".pagedtest.Item")},
				{Name: s("paging"), JsonName: s("paging"), Number: i(2), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: s(".w17.Paging")},
			}},
		},
	}
	f, err := protodesc.NewFile(fd, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	m := f.Messages()
	return m.ByName("Req"), m.ByName("Resp"), m.ByName("Item")
}

// pagedFixture is a paged tool over ten rows, ids 10 down to 1 (ORDER BY id
// DESC), with a backend that serves the keyset the gateway sends it, so a
// cursor followed through the tool must land on the rows a REST client would.
type pagedFixture struct {
	t                    *testing.T
	reqD, respD, itemD   protoreflect.MessageDescriptor
	tool                 *PagedTool
	lastLimit, lastQuery string
	lastBoundaries       bool
}

func newPagedFixture(t *testing.T) *pagedFixture {
	reqD, respD, itemD := pagedTestTypes(t)
	f := &pagedFixture{t: t, reqD: reqD, respD: respD, itemD: itemD}
	f.tool = &PagedTool{
		CursorParam: "cursor", LimitParam: "limit",
		DefaultLimit: 3, MaxLimit: 4, SchemaVersion: 7,
		PagingField: "paging", ItemsField: "items",
	}
	return f
}

// userMD is the metadata an auth function attaches: the auth response with
// the user id in field 2, as every w17 auth response carries it.
func userMD(user string) metadata.MD {
	raw := protowire.AppendTag(nil, 2, protowire.BytesType)
	raw = protowire.AppendString(raw, user)
	return metadata.Pairs("x-w17-user", base64.StdEncoding.EncodeToString(raw), "x-w17-scope-org", "o1")
}

func (f *pagedFixture) call(user string, args map[string]any) (map[string]any, error) {
	return f.callArgs(user, args)
}

// callRaw passes the arguments as the HTTP transport hands them over: the
// raw JSON, captured so 64-bit integers stay exact (exactArgumentHooks).
func (f *pagedFixture) callRaw(user string, args string) (map[string]any, error) {
	return f.callArgs(user, json.RawMessage(args))
}

func (f *pagedFixture) callArgs(user string, args any) (map[string]any, error) {
	s := NewServer("t", "1", func(context.Context, ConnectionInfo) (metadata.MD, error) { return userMD(user), nil })
	s.SetCursorKey([]byte("test-key"))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list"
	req.Params.Arguments = args
	res, err := s.CallPaged(context.Background(), req, dynamicpb.NewMessage(f.reqD), f.tool,
		func(ctx context.Context, reqMsg proto.Message, _ ...grpc.CallOption) (proto.Message, PageBounds, error) {
			resp := f.serve(ctx, reqMsg)
			// What the generated invoke returns: the typed reader over the
			// response it was handed (here through reflection).
			items := resp.ProtoReflect().Get(f.respD.Fields().ByName("items")).List()
			id := f.itemD.Fields().ByName("id")
			return resp, func(i int) []*w17pb.KeysetValue {
				return []*w17pb.KeysetValue{paging.FromInt64(items.Get(i).Message().Get(id).Int())}
			}, nil
		})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out); err != nil {
		f.t.Fatal(err)
	}
	return out, nil
}

// serve is the storage side: LIMIT from metadata, the keyset from metadata
// (id < boundary forward, id > boundary backward, rows then reversed back by
// the gateway), total 10.
func (f *pagedFixture) serve(ctx context.Context, reqMsg proto.Message) proto.Message {
	md, _ := metadata.FromOutgoingContext(ctx)
	in := metadata.NewIncomingContext(ctx, md)
	limit, _ := paging.LimitFromIncomingMD(in)
	f.lastLimit = md.Get(paging.LimitMDKey)[0]
	f.lastQuery = reqMsg.ProtoReflect().Get(f.reqD.Fields().ByName("q")).String()
	b, ok := paging.BoundariesFromIncomingMD(in)
	f.lastBoundaries = ok
	var ids []int64
	switch {
	case !ok:
		for id := int64(10); id >= 1 && uint32(len(ids)) < limit; id-- {
			ids = append(ids, id)
		}
	case b.Direction == w17pb.Direction_DIRECTION_BACKWARD:
		at := paging.ScalarOf(b.Values[0]).(int64)
		for id := at + 1; id <= 10 && uint32(len(ids)) < limit; id++ {
			ids = append(ids, id)
		}
	default:
		at := paging.ScalarOf(b.Values[0]).(int64)
		for id := at - 1; id >= 1 && uint32(len(ids)) < limit; id-- {
			ids = append(ids, id)
		}
	}
	resp := dynamicpb.NewMessage(f.respD)
	list := resp.Mutable(f.respD.Fields().ByName("items")).List()
	for _, id := range ids {
		it := dynamicpb.NewMessage(f.itemD)
		it.Set(f.itemD.Fields().ByName("id"), protoreflect.ValueOfInt64(id))
		list.Append(protoreflect.ValueOfMessage(it))
	}
	pg := resp.Mutable(f.respD.Fields().ByName("paging")).Message()
	pg.Set(pg.Descriptor().Fields().ByName("total"), protoreflect.ValueOfUint64(10))
	return resp
}

func ids(t *testing.T, page map[string]any) []string {
	t.Helper()
	var out []string
	items, _ := page["items"].([]any)
	for _, it := range items {
		out = append(out, it.(map[string]any)["id"].(string))
	}
	return out
}

func cursorOf(page map[string]any, which string) string {
	pg, _ := page["paging"].(map[string]any)
	c, _ := pg[which].(string)
	return c
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The whole walk: page one at the requested size, forward by next_cursor to
// the short last page (which offers no next), and back by previous_cursor to
// the rows before it, in the same order. The cursor restores the page-one
// request, so a filter changed on the follow-up call is not what pages.
func TestCallPaged_WalksForwardAndBackLikeREST(t *testing.T) {
	f := newPagedFixture(t)

	p1, err := f.call("u1", map[string]any{"q": "first", "limit": 4.0})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, p1); !eq(got, []string{"10", "9", "8", "7"}) {
		t.Fatalf("page 1 = %v", got)
	}
	if f.lastLimit != "4" || f.lastBoundaries {
		t.Fatalf("page 1 sent limit %q boundaries %v", f.lastLimit, f.lastBoundaries)
	}
	pg := p1["paging"].(map[string]any)
	if pg["limit"].(float64) != 4 || pg["total"].(string) != "10" || cursorOf(p1, "previous_cursor") != "" || cursorOf(p1, "next_cursor") == "" {
		t.Fatalf("page 1 paging = %v", pg)
	}

	p2, err := f.call("u1", map[string]any{"q": "changed", "cursor": cursorOf(p1, "next_cursor"), "limit": 4.0})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, p2); !eq(got, []string{"6", "5", "4", "3"}) {
		t.Fatalf("page 2 = %v", got)
	}
	if f.lastQuery != "first" {
		t.Fatalf("the cursor must restore the page-one request, backend saw q=%q", f.lastQuery)
	}

	p3, err := f.call("u1", map[string]any{"cursor": cursorOf(p2, "next_cursor"), "limit": 4.0})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, p3); !eq(got, []string{"2", "1"}) {
		t.Fatalf("page 3 = %v", got)
	}
	if cursorOf(p3, "next_cursor") != "" {
		t.Fatal("a short forward page is the last one: no next_cursor")
	}

	back, err := f.call("u1", map[string]any{"cursor": cursorOf(p3, "previous_cursor"), "limit": 4.0})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, back); !eq(got, []string{"6", "5", "4", "3"}) {
		t.Fatalf("back from page 3 = %v, want page 2 in the same order", got)
	}
}

// A full page may have a successor, so it offers next_cursor; when the rows
// end exactly there the next page is empty, and it must still hand back the
// way the caller came rather than strand it with no cursor at all.
func TestCallPaged_AnEmptyResumedPageHandsBackTheWayItCame(t *testing.T) {
	f := newPagedFixture(t)
	f.tool.MaxLimit = 5
	p1, err := f.call("u1", map[string]any{"limit": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := f.call("u1", map[string]any{"cursor": cursorOf(p1, "next_cursor"), "limit": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, p2); !eq(got, []string{"5", "4", "3", "2", "1"}) || cursorOf(p2, "next_cursor") == "" {
		t.Fatalf("page 2 = %v next %q: a full page offers a next", got, cursorOf(p2, "next_cursor"))
	}
	p3, err := f.call("u1", map[string]any{"cursor": cursorOf(p2, "next_cursor"), "limit": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids(t, p3)) != 0 || cursorOf(p3, "next_cursor") != "" || cursorOf(p3, "previous_cursor") == "" {
		t.Fatalf("empty page 3 = %v: want no next and a previous_cursor back", p3["paging"])
	}
	back, err := f.call("u1", map[string]any{"cursor": cursorOf(p3, "previous_cursor"), "limit": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	// The cursor back is positioned at the last row the caller saw (id 1)
	// and excludes it, as the REST handler's is: the rows before it.
	if got := ids(t, back); !eq(got, []string{"6", "5", "4", "3", "2"}) {
		t.Fatalf("back from the empty page = %v", got)
	}
}

// A cursor carries the request it was minted from, so it is the minting
// caller's: another one presenting it is refused, naming the argument.
func TestCallPaged_ACursorOfAnotherCallerIsRefused(t *testing.T) {
	f := newPagedFixture(t)
	p1, err := f.call("u1", map[string]any{"limit": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.call("u2", map[string]any{"cursor": cursorOf(p1, "next_cursor")})
	assertBadArgument(t, err, "cursor")
}

// Over HTTP the arguments arrive as raw JSON, not a decoded map: the paging
// arguments are read from that shape too (they were once ignored there, so a
// tool over the network answered the default page for every limit).
func TestCallPaged_ReadsTheArgumentsTheHTTPTransportHandsOver(t *testing.T) {
	f := newPagedFixture(t)
	p1, err := f.callRaw("u1", `{"limit": 2}`)
	if err != nil {
		t.Fatal(err)
	}
	if f.lastLimit != "2" || len(ids(t, p1)) != 2 || cursorOf(p1, "next_cursor") == "" {
		t.Fatalf("raw limit 2: sent %q, got %v", f.lastLimit, p1["paging"])
	}
	p2, err := f.callRaw("u1", `{"limit": 2, "cursor": "`+cursorOf(p1, "next_cursor")+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, p2); !eq(got, []string{"8", "7"}) {
		t.Fatalf("raw cursor page = %v", got)
	}
	_, err = f.callRaw("u1", `{"limit": "many"}`)
	assertBadArgument(t, err, "limit")
}

func TestCallPaged_DefaultAndClampedLimit(t *testing.T) {
	f := newPagedFixture(t)
	p, err := f.call("u1", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastLimit != "3" || len(ids(t, p)) != 3 {
		t.Fatalf("no limit: sent %q, got %v", f.lastLimit, ids(t, p))
	}
	p, err = f.call("u1", map[string]any{"limit": 1000.0})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastLimit != "4" || p["paging"].(map[string]any)["limit"].(float64) != 4 {
		t.Fatalf("limit above max: sent %q, echoed %v", f.lastLimit, p["paging"])
	}
}

func TestCallPaged_ALimitThatIsNoPageSizeIsRefused(t *testing.T) {
	f := newPagedFixture(t)
	for _, v := range []any{"ten", 2.5, -1.0, true} {
		_, err := f.call("u1", map[string]any{"limit": v})
		assertBadArgument(t, err, "limit")
	}
	_, err := f.call("u1", map[string]any{"cursor": 5.0})
	assertBadArgument(t, err, "cursor")
	_, err = f.call("u1", map[string]any{"cursor": "not-a-cursor"})
	assertBadArgument(t, err, "cursor")
}

func assertBadArgument(t *testing.T, err error, field string) {
	t.Helper()
	st, _ := status.FromError(err)
	if err == nil || st.Code() != codes.InvalidArgument {
		t.Fatalf("want INVALID_ARGUMENT for %s, got %v", field, err)
	}
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok && ed.GetField() == field {
			return
		}
	}
	t.Fatalf("no detail naming %q in %v", field, st.Details())
}
