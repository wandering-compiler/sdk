package kvhash

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func compileFixture(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: []string{"testdata"},
		}),
	}
	files, err := c.Compile(context.Background(), "kvhash_fixture.proto")
	if err != nil {
		t.Fatalf("compile fixture: %v", err)
	}
	f := files.FindFileByPath("kvhash_fixture.proto")
	if f == nil {
		t.Fatal("kvhash_fixture.proto not found in compile output")
	}
	return f
}

func msgByName(t *testing.T, fd protoreflect.FileDescriptor, name string) protoreflect.MessageDescriptor {
	t.Helper()
	md := fd.Messages().ByName(protoreflect.Name(name))
	if md == nil {
		t.Fatalf("message %q not found", name)
	}
	return md
}

func setField(m protoreflect.Message, fd protoreflect.FieldDescriptor, v protoreflect.Value) {
	m.Set(fd, v)
}

func findField(t *testing.T, md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	t.Helper()
	fd := md.Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		t.Fatalf("field %q not found on %s", name, md.FullName())
	}
	return fd
}

// TestMarshalEntity_FlatScalars covers every scalar kind: each
// produces the expected protojson-equivalent string in the hash.
func TestMarshalEntity_FlatScalars(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	m := dynamicpb.NewMessage(md)
	rm := m.ProtoReflect()

	setField(rm, findField(t, md, "id"), protoreflect.ValueOfString("abc"))
	setField(rm, findField(t, md, "active"), protoreflect.ValueOfBool(true))
	setField(rm, findField(t, md, "score32"), protoreflect.ValueOfInt32(-7))
	setField(rm, findField(t, md, "score64"), protoreflect.ValueOfInt64(123456789012))
	setField(rm, findField(t, md, "count32"), protoreflect.ValueOfUint32(42))
	setField(rm, findField(t, md, "count64"), protoreflect.ValueOfUint64(99999999999))
	setField(rm, findField(t, md, "ratio32"), protoreflect.ValueOfFloat32(1.5))
	setField(rm, findField(t, md, "ratio64"), protoreflect.ValueOfFloat64(2.71828))
	setField(rm, findField(t, md, "payload"), protoreflect.ValueOfBytes([]byte{0x01, 0x02, 0xff}))
	statusFd := findField(t, md, "status")
	setField(rm, statusFd, protoreflect.ValueOfEnum(2)) // DONE
	tsFd := findField(t, md, "created_at")
	tsMsg := timestamppb.New(time.Date(2026, 5, 6, 12, 30, 45, 123000000, time.UTC))
	setField(rm, tsFd, protoreflect.ValueOfMessage(tsMsg.ProtoReflect()))
	durFd := findField(t, md, "ttl")
	durMsg := durationpb.New(1500 * time.Millisecond)
	setField(rm, durFd, protoreflect.ValueOfMessage(durMsg.ProtoReflect()))

	got, err := MarshalEntity(m)
	if err != nil {
		t.Fatalf("MarshalEntity: %v", err)
	}

	// Flatten to map for assertion clarity.
	hash := pairsToMap(t, got)

	want := map[string]string{
		"id":         "abc",
		"active":     "true",
		"score32":    "-7",
		"score64":    "123456789012",
		"count32":    "42",
		"count64":    "99999999999",
		"ratio32":    "1.5",
		"ratio64":    "2.71828",
		"payload":    base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0xff}),
		"status":     "DONE",
		"created_at": "2026-05-06T12:30:45.123Z",
		"ttl":        "1.5s",
	}

	for k, v := range want {
		if hash[k] != v {
			t.Errorf("field %q: got %q, want %q", k, hash[k], v)
		}
	}
}

// TestMarshalUnmarshalRoundTrip — write a flat entity, decode it
// back, expect proto.Equal.
func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	src := dynamicpb.NewMessage(md)
	rm := src.ProtoReflect()
	setField(rm, findField(t, md, "id"), protoreflect.ValueOfString("xyz"))
	setField(rm, findField(t, md, "active"), protoreflect.ValueOfBool(false))
	setField(rm, findField(t, md, "score64"), protoreflect.ValueOfInt64(-1))
	setField(rm, findField(t, md, "ratio64"), protoreflect.ValueOfFloat64(3.14))
	setField(rm, findField(t, md, "payload"), protoreflect.ValueOfBytes([]byte("hi")))
	setField(rm, findField(t, md, "status"), protoreflect.ValueOfEnum(1))
	ts := timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	setField(rm, findField(t, md, "created_at"), protoreflect.ValueOfMessage(ts.ProtoReflect()))
	dur := durationpb.New(2 * time.Second)
	setField(rm, findField(t, md, "ttl"), protoreflect.ValueOfMessage(dur.ProtoReflect()))

	pairs, err := MarshalEntity(src)
	if err != nil {
		t.Fatalf("MarshalEntity: %v", err)
	}
	hash := pairsToMap(t, pairs)

	dst := dynamicpb.NewMessage(md)
	if err := UnmarshalEntity(hash, dst); err != nil {
		t.Fatalf("UnmarshalEntity: %v", err)
	}
	if !proto.Equal(src, dst) {
		t.Fatalf("round trip mismatch:\n src=%v\n dst=%v", src, dst)
	}
}

// TestMarshalEntity_RefusesNested — nested-message field other
// than Timestamp/Duration is an error pointing at the offending
// field.
func TestMarshalEntity_RefusesNested(t *testing.T) {
	md := msgByName(t, compileFixture(t), "NestedEntity")
	m := dynamicpb.NewMessage(md)
	_, err := MarshalEntity(m)
	if err == nil {
		t.Fatal("expected error for nested message field")
	}
	if !contains(err.Error(), `"inner"`) {
		t.Errorf("error should name the offending field: %v", err)
	}
}

func TestMarshalEntity_RefusesRepeated(t *testing.T) {
	md := msgByName(t, compileFixture(t), "RepeatedEntity")
	m := dynamicpb.NewMessage(md)
	_, err := MarshalEntity(m)
	if err == nil {
		t.Fatal("expected error for repeated field")
	}
	if !contains(err.Error(), `"tags"`) || !contains(err.Error(), "repeated") {
		t.Errorf("error should explain the repeated rejection: %v", err)
	}
}

func TestMarshalEntity_RefusesMap(t *testing.T) {
	md := msgByName(t, compileFixture(t), "MapEntity")
	m := dynamicpb.NewMessage(md)
	_, err := MarshalEntity(m)
	if err == nil {
		t.Fatal("expected error for map field")
	}
	if !contains(err.Error(), `"attributes"`) || !contains(err.Error(), "map") {
		t.Errorf("error should explain the map rejection: %v", err)
	}
}

// TestUnmarshalEntity_MissingFieldsKeepDefaults — partial hash
// only sets the present fields; absent fields stay at zero.
func TestUnmarshalEntity_MissingFieldsKeepDefaults(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	dst := dynamicpb.NewMessage(md)
	hash := map[string]string{
		"id":      "only-id",
		"score64": "777",
	}
	if err := UnmarshalEntity(hash, dst); err != nil {
		t.Fatalf("UnmarshalEntity: %v", err)
	}
	rm := dst.ProtoReflect()
	if got := rm.Get(findField(t, md, "id")).String(); got != "only-id" {
		t.Errorf("id: got %q", got)
	}
	if got := rm.Get(findField(t, md, "score64")).Int(); got != 777 {
		t.Errorf("score64: got %d", got)
	}
	if got := rm.Get(findField(t, md, "active")).Bool(); got {
		t.Errorf("active: should default false, got true")
	}
}

// TestUnmarshalEntity_InvalidValues — bad bool, bad int, bad
// timestamp, bad enum each produce a typed error.
func TestUnmarshalEntity_InvalidValues(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	cases := []struct {
		name string
		hash map[string]string
		want string
	}{
		{name: "bool", hash: map[string]string{"active": "yes"}, want: "invalid bool"},
		{name: "int", hash: map[string]string{"score64": "not-a-number"}, want: "invalid int64"},
		{name: "timestamp", hash: map[string]string{"created_at": "not-a-date"}, want: "invalid RFC3339Nano timestamp"},
		{name: "enum", hash: map[string]string{"status": "BOGUS"}, want: "invalid enum value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := dynamicpb.NewMessage(md)
			err := UnmarshalEntity(tc.hash, dst)
			if err == nil {
				t.Fatal("expected error")
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error should contain %q: %v", tc.want, err)
			}
		})
	}
}

// TestUnmarshalEntity_EnumNumericFallback — older writers emit
// numeric form for forward compat; reader still accepts.
func TestUnmarshalEntity_EnumNumericFallback(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	dst := dynamicpb.NewMessage(md)
	if err := UnmarshalEntity(map[string]string{"status": "1"}, dst); err != nil {
		t.Fatalf("UnmarshalEntity: %v", err)
	}
	got := dst.ProtoReflect().Get(findField(t, md, "status")).Enum()
	if got != 1 {
		t.Errorf("status: got %d, want 1", got)
	}
}

// TestMarshalEntity_Proto3OptionalPresence pins Q7-kvhash-1: a
// proto3-`optional` scalar carries presence (synthetic oneof), so
// an UNSET optional must be omitted from the HASH — distinguishing
// it from an optional explicitly SET TO ZERO — while a plain proto3
// scalar (no presence) is always emitted. The round-trip must
// preserve the set/unset distinction.
func TestMarshalEntity_Proto3OptionalPresence(t *testing.T) {
	md := msgByName(t, compileFixture(t), "OptionalEntity")
	nameFd := findField(t, md, "name")
	ageFd := findField(t, md, "age")

	// Sanity: the fixture really gives `age` presence and `name` none.
	if !ageFd.HasPresence() {
		t.Fatal("fixture invalid: optional int32 age should have presence")
	}
	if nameFd.HasPresence() {
		t.Fatal("fixture invalid: plain string name should not have presence")
	}

	// Case 1: optional UNSET — the "age" key must be omitted, while
	// the plain scalar "name" is present (as its zero "").
	t.Run("UnsetOptionalOmitted", func(t *testing.T) {
		m := dynamicpb.NewMessage(md)
		pairs, err := MarshalEntity(m)
		if err != nil {
			t.Fatalf("MarshalEntity: %v", err)
		}
		hash := pairsToMap(t, pairs)
		if _, ok := hash["age"]; ok {
			t.Errorf("unset optional should be omitted; got age=%q", hash["age"])
		}
		if v, ok := hash["name"]; !ok || v != "" {
			t.Errorf("plain scalar name should be present as \"\"; got %q (present=%v)", v, ok)
		}
	})

	// Case 2: optional SET TO ZERO — the key must be present with the
	// zero value, proving set-zero is distinguished from unset.
	t.Run("SetZeroOptionalPresent", func(t *testing.T) {
		m := dynamicpb.NewMessage(md)
		m.ProtoReflect().Set(ageFd, protoreflect.ValueOfInt32(0))
		pairs, err := MarshalEntity(m)
		if err != nil {
			t.Fatalf("MarshalEntity: %v", err)
		}
		hash := pairsToMap(t, pairs)
		if v, ok := hash["age"]; !ok || v != "0" {
			t.Errorf("set-zero optional should be present as \"0\"; got %q (present=%v)", v, ok)
		}
	})

	// Case 3: round-trip Marshal(unset) -> Unmarshal — the optional
	// must read back as UNSET (absent key leaves it absent).
	t.Run("RoundTripUnset", func(t *testing.T) {
		src := dynamicpb.NewMessage(md)
		src.ProtoReflect().Set(nameFd, protoreflect.ValueOfString("ada"))
		pairs, err := MarshalEntity(src)
		if err != nil {
			t.Fatalf("MarshalEntity: %v", err)
		}
		dst := dynamicpb.NewMessage(md)
		if err := UnmarshalEntity(pairsToMap(t, pairs), dst); err != nil {
			t.Fatalf("UnmarshalEntity: %v", err)
		}
		if dst.ProtoReflect().Has(ageFd) {
			t.Errorf("round-tripped unset optional should stay unset, got present value %d",
				dst.ProtoReflect().Get(ageFd).Int())
		}
		if !proto.Equal(src, dst) {
			t.Errorf("round trip mismatch:\n src=%v\n dst=%v", src, dst)
		}
	})

	// Case 4: round-trip Marshal(set-zero) -> Unmarshal — the optional
	// must read back as a PRESENT zero.
	t.Run("RoundTripSetZero", func(t *testing.T) {
		src := dynamicpb.NewMessage(md)
		src.ProtoReflect().Set(ageFd, protoreflect.ValueOfInt32(0))
		pairs, err := MarshalEntity(src)
		if err != nil {
			t.Fatalf("MarshalEntity: %v", err)
		}
		dst := dynamicpb.NewMessage(md)
		if err := UnmarshalEntity(pairsToMap(t, pairs), dst); err != nil {
			t.Fatalf("UnmarshalEntity: %v", err)
		}
		if !dst.ProtoReflect().Has(ageFd) {
			t.Error("round-tripped set-zero optional should be present, got unset")
		}
		if got := dst.ProtoReflect().Get(ageFd).Int(); got != 0 {
			t.Errorf("set-zero optional value: got %d, want 0", got)
		}
		if !proto.Equal(src, dst) {
			t.Errorf("round trip mismatch:\n src=%v\n dst=%v", src, dst)
		}
	})
}

func pairsToMap(t *testing.T, pairs []any) map[string]string {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatalf("odd-length pairs slice: %d", len(pairs))
	}
	out := make(map[string]string, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			t.Fatalf("pairs[%d] is not a string field name: %T", i, pairs[i])
		}
		v, ok := pairs[i+1].(string)
		if !ok {
			t.Fatalf("pairs[%d] is not a string value: %T", i+1, pairs[i+1])
		}
		out[k] = v
	}
	return out
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
