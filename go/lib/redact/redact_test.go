package redact

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testProto = `
syntax = "proto3";
package redacttest;
import "google/protobuf/any.proto";
import "google/protobuf/timestamp.proto";

message Req {
  string name = 1;
  string password = 2;   // sensitive
  bytes blob = 3;
  repeated Item items = 4;
  map<string, string> tags = 5;
  Profile profile = 6;
  repeated int64 counts = 7;  // packed
  oneof pick {
    string code = 8;
    int32 number = 9;
  }
  google.protobuf.Any extra = 10;
  google.protobuf.Timestamp at = 11;
  Opaque opaque = 12;            // no plan: left out
  optional int32 maybe = 13;
  double ratio = 14;
  fixed32 f32 = 15;
  sint64 s64 = 16;
  bool flag = 17;
  repeated string notes = 18;
}
message Item { string title = 1; string secret = 2; }
message Profile { string bio = 1; string token = 2; Item best = 3; }
message Opaque { string anything = 1; }
`

func compile(t testing.TB) protoreflect.FileDescriptor {
	t.Helper()
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(map[string]string{"t.proto": testProto}),
	})}
	files, err := c.Compile(context.Background(), "t.proto")
	if err != nil {
		t.Fatal(err)
	}
	return files[0]
}

func testSet() *Set {
	return NewSet(map[string]Plan{
		".redacttest.Req":    {Skip: []protowire.Number{2}},
		"redacttest.Item":    {Skip: []protowire.Number{2}},
		"redacttest.Profile": {Skip: []protowire.Number{2}},
	})
}

type msgs struct {
	req, item, profile, opaque protoreflect.MessageDescriptor
}

func descs(t testing.TB) msgs {
	fd := compile(t)
	m := fd.Messages()
	return msgs{m.ByName("Req"), m.ByName("Item"), m.ByName("Profile"), m.ByName("Opaque")}
}

func set(m *dynamicpb.Message, name string, v protoreflect.Value) {
	m.Set(m.Descriptor().Fields().ByName(protoreflect.Name(name)), v)
}

// sample fills every field, the sensitive ones with the decoy "DECOY".
func sample(t testing.TB, d msgs) *dynamicpb.Message {
	t.Helper()
	req := dynamicpb.NewMessage(d.req)
	set(req, "name", protoreflect.ValueOfString("Jana"))
	set(req, "password", protoreflect.ValueOfString("DECOY-pw"))
	set(req, "blob", protoreflect.ValueOfBytes([]byte{1, 2, 3}))
	items := req.Mutable(d.req.Fields().ByName("items")).List()
	for _, title := range []string{"a", "b"} {
		it := dynamicpb.NewMessage(d.item)
		set(it, "title", protoreflect.ValueOfString(title))
		set(it, "secret", protoreflect.ValueOfString("DECOY-item"))
		items.Append(protoreflect.ValueOfMessage(it))
	}
	tags := req.Mutable(d.req.Fields().ByName("tags")).Map()
	tags.Set(protoreflect.ValueOfString("k").MapKey(), protoreflect.ValueOfString("v"))
	prof := dynamicpb.NewMessage(d.profile)
	set(prof, "bio", protoreflect.ValueOfString("hi"))
	set(prof, "token", protoreflect.ValueOfString("DECOY-token"))
	best := dynamicpb.NewMessage(d.item)
	set(best, "title", protoreflect.ValueOfString("best"))
	set(best, "secret", protoreflect.ValueOfString("DECOY-best"))
	set(prof, "best", protoreflect.ValueOfMessage(best))
	set(req, "profile", protoreflect.ValueOfMessage(prof))
	counts := req.Mutable(d.req.Fields().ByName("counts")).List()
	for _, n := range []int64{1, -1, 300} {
		counts.Append(protoreflect.ValueOfInt64(n))
	}
	set(req, "number", protoreflect.ValueOfInt32(7))
	extra, _ := anypb.New(timestamppb.New(timestamppb.Now().AsTime()))
	extraMsg := dynamicpb.NewMessage(d.req.Fields().ByName("extra").Message())
	b, _ := proto.Marshal(extra)
	if err := proto.Unmarshal(b, extraMsg); err != nil {
		t.Fatal(err)
	}
	set(req, "extra", protoreflect.ValueOfMessage(extraMsg))
	at := dynamicpb.NewMessage(d.req.Fields().ByName("at").Message())
	set(at, "seconds", protoreflect.ValueOfInt64(1700000000))
	set(req, "at", protoreflect.ValueOfMessage(at))
	op := dynamicpb.NewMessage(d.opaque)
	set(op, "anything", protoreflect.ValueOfString("DECOY-opaque"))
	set(req, "opaque", protoreflect.ValueOfMessage(op))
	set(req, "maybe", protoreflect.ValueOfInt32(0))
	set(req, "ratio", protoreflect.ValueOfFloat64(0.5))
	set(req, "f32", protoreflect.ValueOfUint32(9))
	set(req, "s64", protoreflect.ValueOfInt64(-5))
	set(req, "flag", protoreflect.ValueOfBool(true))
	// An unknown field: a newer client's field the gateway cannot classify.
	req.SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 99, protowire.BytesType), "DECOY-unknown"))
	return req
}

func TestAppend_LeavesOutWhatMayNotLeave(t *testing.T) {
	d := descs(t)
	req := sample(t, d)
	out, cut, err := testSet().Append(nil, req, 0)
	if err != nil || cut != nil {
		t.Fatalf("err=%v cut=%v", err, cut)
	}
	if bytes.Contains(out, []byte("DECOY")) {
		t.Fatalf("a left-out value reached the encoding: %q", out)
	}

	got := dynamicpb.NewMessage(d.req)
	if err := proto.Unmarshal(out, got); err != nil {
		t.Fatalf("output is not a valid Req: %v", err)
	}
	want := proto.Clone(req).(*dynamicpb.Message)
	want.SetUnknown(nil)
	for _, f := range []string{"password", "extra", "opaque"} {
		want.Clear(d.req.Fields().ByName(protoreflect.Name(f)))
	}
	items := want.Get(d.req.Fields().ByName("items")).List()
	for i := 0; i < items.Len(); i++ {
		items.Get(i).Message().Clear(d.item.Fields().ByName("secret"))
	}
	prof := want.Get(d.req.Fields().ByName("profile")).Message()
	prof.Clear(d.profile.Fields().ByName("token"))
	prof.Get(d.profile.Fields().ByName("best")).Message().Clear(d.item.Fields().ByName("secret"))
	if !proto.Equal(got, want) {
		t.Fatalf("decoded record differs from the request minus the left-out fields\n got: %v\nwant: %v", got, want)
	}
	// Presence survives: an optional set to its zero is still set.
	if !got.Has(d.req.Fields().ByName("maybe")) {
		t.Fatal("proto3 optional set to 0 lost its presence")
	}
}

func TestAppend_NoPlan(t *testing.T) {
	d := descs(t)
	_, _, err := testSet().Append([]byte("x"), dynamicpb.NewMessage(d.opaque), 0)
	if !errors.Is(err, ErrNoPlan) {
		t.Fatalf("err = %v, want ErrNoPlan", err)
	}
}

func TestAppend_CutsTheLargestFirst(t *testing.T) {
	d := descs(t)
	req := dynamicpb.NewMessage(d.req)
	big := strings.Repeat("ž", 40_000) // 80 000 bytes, two per rune
	set(req, "name", protoreflect.ValueOfString(big))
	set(req, "blob", protoreflect.ValueOfBytes([]byte("small")))
	notes := req.Mutable(d.req.Fields().ByName("notes")).List()
	for i := 0; i < 2000; i++ {
		notes.Append(protoreflect.ValueOfString("note-0123456789"))
	}
	const ceiling = 32 << 10
	prefix := []byte("envelope")
	out, cut, err := testSet().Append(prefix, req, ceiling)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, prefix) {
		t.Fatal("dst prefix lost")
	}
	body := out[len(prefix):]
	if len(body) > ceiling {
		t.Fatalf("record %d bytes, ceiling %d", len(body), ceiling)
	}
	if len(body) < ceiling-64 {
		t.Fatalf("record cut to %d bytes, far below the ceiling %d: more was cut than needed", len(body), ceiling)
	}
	if strings.Join(cut, ",") != "name,notes" {
		t.Fatalf("cut = %v, want [name notes]", cut)
	}
	got := dynamicpb.NewMessage(d.req)
	if err := proto.Unmarshal(body, got); err != nil {
		t.Fatal(err)
	}
	name := got.Get(d.req.Fields().ByName("name")).String()
	if !utf8.ValidString(name) || !strings.HasPrefix(big, name) || name == "" {
		t.Fatalf("name cut to an invalid or foreign prefix (%d bytes)", len(name))
	}
	if string(got.Get(d.req.Fields().ByName("blob")).Bytes()) != "small" {
		t.Fatal("a small field under the level was cut")
	}
	n := got.Get(d.req.Fields().ByName("notes")).List().Len()
	if n == 0 || n == 2000 {
		t.Fatalf("notes kept %d of 2000", n)
	}
	// Same level for both: the two big units end up within one entry's size.
	nameLen := protowire.SizeTag(1) + protowire.SizeBytes(len(name))
	notesLen := n * (protowire.SizeTag(18) + protowire.SizeBytes(len("note-0123456789")))
	if diff := nameLen - notesLen; diff < -32 || diff > 32 {
		t.Fatalf("name %d bytes vs notes %d bytes: not one common level", nameLen, notesLen)
	}
}

func TestAppend_CutsInsideNestedMessagesButNotInsideLists(t *testing.T) {
	d := descs(t)
	req := dynamicpb.NewMessage(d.req)
	prof := dynamicpb.NewMessage(d.profile)
	set(prof, "bio", protoreflect.ValueOfString(strings.Repeat("b", 50_000)))
	set(req, "profile", protoreflect.ValueOfMessage(prof))
	items := req.Mutable(d.req.Fields().ByName("items")).List()
	for i := 0; i < 3; i++ {
		it := dynamicpb.NewMessage(d.item)
		set(it, "title", protoreflect.ValueOfString(strings.Repeat("t", 10_000)))
		items.Append(protoreflect.ValueOfMessage(it))
	}
	out, cut, err := testSet().Append(nil, req, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 16<<10 {
		t.Fatalf("record %d bytes", len(out))
	}
	if strings.Join(cut, ",") != "items,profile.bio" {
		t.Fatalf("cut = %v", cut)
	}
	got := dynamicpb.NewMessage(d.req)
	if err := proto.Unmarshal(out, got); err != nil {
		t.Fatal(err)
	}
	// List entries are whole: each kept item has its full title.
	l := got.Get(d.req.Fields().ByName("items")).List()
	for i := 0; i < l.Len(); i++ {
		if n := len(l.Get(i).Message().Get(d.item.Fields().ByName("title")).String()); n != 10_000 {
			t.Fatalf("item %d title cut to %d", i, n)
		}
	}
}

func TestAppend_PackedAndMapCut(t *testing.T) {
	d := descs(t)
	req := dynamicpb.NewMessage(d.req)
	counts := req.Mutable(d.req.Fields().ByName("counts")).List()
	for i := 0; i < 20_000; i++ {
		counts.Append(protoreflect.ValueOfInt64(int64(i)))
	}
	tags := req.Mutable(d.req.Fields().ByName("tags")).Map()
	for i := 0; i < 2000; i++ {
		tags.Set(protoreflect.ValueOfString(strings.Repeat("k", 8)+string(rune('a'+i%26))+strings.Repeat("x", i%7)+"-"+itoa(i)).MapKey(), protoreflect.ValueOfString("value"))
	}
	out, cut, err := testSet().Append(nil, req, 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 8<<10 {
		t.Fatalf("record %d bytes", len(out))
	}
	if len(cut) != 2 {
		t.Fatalf("cut = %v", cut)
	}
	got := dynamicpb.NewMessage(d.req)
	if err := proto.Unmarshal(out, got); err != nil {
		t.Fatal(err)
	}
	c := got.Get(d.req.Fields().ByName("counts")).List()
	for i := 0; i < c.Len(); i++ {
		if c.Get(i).Int() != int64(i) {
			t.Fatalf("packed list not a prefix at %d", i)
		}
	}
}

func TestAppend_TooLarge(t *testing.T) {
	d := descs(t)
	req := sample(t, d)
	_, _, err := testSet().Append(nil, req, 4)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestAppend_ManyCeilings(t *testing.T) {
	d := descs(t)
	req := sample(t, d)
	set(req, "name", protoreflect.ValueOfString(strings.Repeat("é", 3000)))
	set(req, "blob", protoreflect.ValueOfBytes(bytes.Repeat([]byte{7}, 5000)))
	full, _, _ := testSet().Append(nil, req, 0)
	for ceiling := 200; ceiling <= len(full)+10; ceiling += 37 {
		out, _, err := testSet().Append(nil, req, ceiling)
		if err != nil {
			t.Fatalf("ceiling %d: %v", ceiling, err)
		}
		if len(out) > ceiling {
			t.Fatalf("ceiling %d: %d bytes", ceiling, len(out))
		}
		got := dynamicpb.NewMessage(d.req)
		if err := proto.Unmarshal(out, got); err != nil {
			t.Fatalf("ceiling %d: %v", ceiling, err)
		}
		if !utf8.ValidString(got.Get(d.req.Fields().ByName("name")).String()) {
			t.Fatalf("ceiling %d: name not UTF-8", ceiling)
		}
	}
}

func TestWaterLevel(t *testing.T) {
	for _, c := range []struct {
		units  []int
		excess int
		level  int
		ok     bool
	}{
		{[]int{100}, 30, 70, true},
		{[]int{100, 10}, 30, 70, true},
		{[]int{100, 80}, 40, 70, true},
		{[]int{100, 100, 100}, 30, 90, true},
		{[]int{10, 10}, 25, 0, false},
		{[]int{10, 10}, 20, 0, true},
	} {
		level, ok := waterLevel(c.units, c.excess)
		if level != c.level || ok != c.ok {
			t.Errorf("waterLevel(%v, %d) = %d, %v; want %d, %v", c.units, c.excess, level, ok, c.level, c.ok)
		}
	}
}

func TestAppend_NoAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are meaningless under -race")
	}
	d := descs(t)
	req := sample(t, d)
	req.Clear(d.req.Fields().ByName("tags")) // a map's Range takes a closure
	s := testSet()
	buf := make([]byte, 0, 4096)
	allocs := testing.AllocsPerRun(100, func() {
		buf, _, _ = s.Append(buf[:0], req, 64<<10)
	})
	if allocs != 0 {
		t.Fatalf("%v allocations per record, want 0", allocs)
	}
}

func itoa(i int) string {
	var b [20]byte
	n := len(b)
	for {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
		if i == 0 {
			return string(b[n:])
		}
	}
}

func BenchmarkAppend(b *testing.B) {
	d := descs(b)
	req := sample(b, d)
	s := testSet()
	buf := make([]byte, 0, 4096)
	b.ReportAllocs()
	for b.Loop() {
		buf, _, _ = s.Append(buf[:0], req, 64<<10)
	}
}

func TestRegister_AddsAndReplaces(t *testing.T) {
	d := descs(t)
	s := &Set{}
	if _, _, err := s.Append(nil, dynamicpb.NewMessage(d.item), 0); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("empty set: err = %v", err)
	}
	s.Register(map[string]Plan{"redacttest.Item": {}})
	s.Register(map[string]Plan{"redacttest.Profile": {}})
	it := dynamicpb.NewMessage(d.item)
	set(it, "secret", protoreflect.ValueOfString("DECOY"))
	out, _, err := s.Append(nil, it, 0)
	if err != nil || !bytes.Contains(out, []byte("DECOY")) {
		t.Fatalf("first registration lost or plan not applied: %q %v", out, err)
	}
	s.Register(map[string]Plan{"redacttest.Item": {Skip: []protowire.Number{2}}})
	out, _, _ = s.Append(nil, it, 0)
	if bytes.Contains(out, []byte("DECOY")) {
		t.Fatal("a re-registered plan did not replace the old one")
	}
	if _, _, err := s.Append(nil, dynamicpb.NewMessage(d.profile), 0); err != nil {
		t.Fatalf("the other registration was dropped: %v", err)
	}
}
