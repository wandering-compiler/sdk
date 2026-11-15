package fileref_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/fileref"
)

// fakeTier answers w17.FileRefs/Readable from a set of readable keys (or
// with err) and records every other call that reaches it.
type fakeTier struct {
	readable map[string]bool
	err      error
	asked    []string
	sent     []string
}

func (f *fakeTier) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	if method == "/w17.FileRefs/Readable" {
		key := args.(*wrapperspb.StringValue).GetValue()
		f.asked = append(f.asked, key)
		if f.err != nil {
			return f.err
		}
		reply.(*wrapperspb.BoolValue).Value = f.readable[key]
		return nil
	}
	f.sent = append(f.sent, method)
	return nil
}

func (f *fakeTier) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return &fakeStream{f: f}, nil
}

type fakeStream struct {
	grpc.ClientStream
	f *fakeTier
}

func (s *fakeStream) SendMsg(any) error { s.f.sent = append(s.f.sent, "stream"); return nil }

// The claimable fields, on stand-in messages: DescriptorProto.name is an
// uploaded field of a gated bucket; DescriptorProto.field holds messages
// whose default_value is one.
var claims = fileref.Claims{
	"google.protobuf.DescriptorProto": {
		{Field: "name"},
		{Field: "field", Message: true},
	},
	"google.protobuf.FieldDescriptorProto": {{Field: "default_value"}},
}

// TestGuardClaims: a request may carry a handle in an uploaded field of a
// gated bucket only when the gateway minted it in this request, when it is
// empty, or when the caller may already read the file. Anything else — a
// key learned elsewhere, written into the caller's own row — is refused
// before it reaches the tier, on unary calls and streams alike.
func TestGuardClaims(t *testing.T) {
	ctx := context.Background()
	msg := func(top string, nested ...string) proto.Message {
		m := &descriptorpb.DescriptorProto{Name: proto.String(top)}
		for _, n := range nested {
			m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{DefaultValue: proto.String(n)})
		}
		return m
	}
	cases := []struct {
		name    string
		ctx     context.Context
		msg     proto.Message
		refused string // the path the refusal names; "" = sent
	}{
		{"a file the caller may read (its row's current value)", ctx, msg("docs/mine"), ""},
		{"another organization's file", ctx, msg("docs/theirs"), "name"},
		{"cleared", ctx, msg(""), ""},
		{"minted by this request's upload", fileref.WithMinted(ctx, "docs/fresh"), msg("docs/fresh"), ""},
		{"nested, repeated: one claimed", ctx, msg("docs/mine", "docs/mine", "docs/theirs"), "field.default_value"},
		{"a message without claimable fields", ctx, wrapperspb.String("docs/theirs"), ""},
	}
	for _, c := range cases {
		tier := &fakeTier{readable: map[string]bool{"docs/mine": true}}
		storage, business := fileref.GuardClaims(tier, tier, claims)
		if storage != business {
			t.Fatal("one tier conn became two")
		}
		err := business.Invoke(c.ctx, "/app.NoteMutation/Update", c.msg, &wrapperspb.BoolValue{})
		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.refused != "" && (status.Code(err) != codes.InvalidArgument || !strings.HasPrefix(status.Convert(err).Message(), c.refused+":")):
			t.Errorf("%s: err = %v, want InvalidArgument naming %s", c.name, err, c.refused)
		case c.refused != "" && len(tier.sent) != 0:
			t.Errorf("%s: the refused request reached the tier", c.name)
		}
	}

	// A stream checks every message it sends.
	tier := &fakeTier{readable: map[string]bool{"docs/mine": true}}
	storage, _ := fileref.GuardClaims(tier, &fakeTier{}, claims)
	st, err := storage.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, "/app.NoteMutation/Import")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SendMsg(msg("docs/mine")); err != nil {
		t.Errorf("stream, readable: %v", err)
	}
	if err := st.SendMsg(msg("docs/theirs")); status.Code(err) != codes.InvalidArgument {
		t.Errorf("stream, another organization's file: %v", err)
	}
	if len(tier.sent) != 1 {
		t.Errorf("the stream sent %d messages, want the readable one only", len(tier.sent))
	}

	// The business tier is guarded too, and asks the STORAGE tier.
	refs, biz := &fakeTier{readable: map[string]bool{}}, &fakeTier{}
	_, business := fileref.GuardClaims(refs, biz, claims)
	if err := business.Invoke(ctx, "/app.Facade/Attach", msg("docs/theirs"), &wrapperspb.BoolValue{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("business tier: %v", err)
	}
	if len(refs.asked) != 1 || len(biz.sent) != 0 {
		t.Errorf("business tier: storage asked %v, business sent %v", refs.asked, biz.sent)
	}

	// A failure to ask is not an answer: it is returned, not read as "no".
	down := &fakeTier{err: status.Error(codes.Unavailable, "storage down")}
	g, _ := fileref.GuardClaims(down, down, claims)
	if err := g.Invoke(ctx, "/app.NoteMutation/Update", msg("docs/x"), &wrapperspb.BoolValue{}); status.Code(err) != codes.Unavailable {
		t.Errorf("storage down: %v", err)
	}
	// A refusal to answer (no scope, a malformed key) reads as "no".
	denied := &fakeTier{err: status.Error(codes.PermissionDenied, "no")}
	g, _ = fileref.GuardClaims(denied, denied, claims)
	if err := g.Invoke(ctx, "/app.NoteMutation/Update", msg("docs/x"), &wrapperspb.BoolValue{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("refused answer: %v", err)
	}
	// No claims: the conns are returned as they are.
	if s, _ := fileref.GuardClaims(tier, tier, nil); s != grpc.ClientConnInterface(tier) {
		t.Error("an empty table still wrapped the conns")
	}
}
