package kvhash

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A Timestamp RFC 3339 cannot spell is refused on write, so no entity is
// stored that every later read would fail on.
func TestMarshalEntity_RefusesATimestampThatCannotBeReadBack(t *testing.T) {
	md := msgByName(t, compileFixture(t), "FlatEntity")
	fd := findField(t, md, "created_at")
	for name, ts := range map[string]*timestamppb.Timestamp{
		"after year 9999":  {Seconds: 253402387200},
		"before year 0001": {Seconds: -62135596801},
		"bad nanos":        {Seconds: 1, Nanos: 1_000_000_000},
	} {
		m := dynamicpb.NewMessage(md)
		m.Set(fd, protoreflect.ValueOfMessage(ts.ProtoReflect()))
		if _, err := MarshalEntity(m); err == nil {
			t.Errorf("%s: a Timestamp that cannot be read back was accepted", name)
		}
	}

	// The edges of the range still round-trip.
	for _, ts := range []*timestamppb.Timestamp{{Seconds: 253402300799, Nanos: 999999999}, {Seconds: -62135596800}} {
		m := dynamicpb.NewMessage(md)
		m.Set(fd, protoreflect.ValueOfMessage(ts.ProtoReflect()))
		pairs, err := MarshalEntity(m)
		if err != nil {
			t.Fatalf("%v: %v", ts, err)
		}
		back := dynamicpb.NewMessage(md)
		if err := UnmarshalEntity(pairsToMap(t, pairs), back); err != nil {
			t.Fatalf("%v: read back: %v", ts, err)
		}
	}
}

// Clearing an optional field on update removes the stored value: the write
// HDELs the fields ClearedFields names in the same MULTI as the HSET, and the
// cleared value does not read back.
func TestClearedFields_AnUpdateThatClearsAFieldRemovesIt(t *testing.T) {
	md := msgByName(t, compileFixture(t), "OptionalEntity")
	ageFd := findField(t, md, "age")
	nameFd := findField(t, md, "name")

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	write := func(m protoreflect.ProtoMessage) {
		t.Helper()
		pairs, err := MarshalEntity(m)
		if err != nil {
			t.Fatal(err)
		}
		cleared, err := ClearedFields(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, "e:1", pairs...)
			if len(cleared) > 0 {
				pipe.HDel(ctx, "e:1", cleared...)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	first := dynamicpb.NewMessage(md)
	first.Set(nameFd, protoreflect.ValueOfString("a"))
	first.Set(ageFd, protoreflect.ValueOfInt32(41))
	write(first)

	second := dynamicpb.NewMessage(md)
	second.Set(nameFd, protoreflect.ValueOfString("a"))
	if got, _ := ClearedFields(second); len(got) != 1 || got[0] != "age" {
		t.Fatalf("ClearedFields = %v, want [age]", got)
	}
	write(second)

	hash, err := rdb.HGetAll(ctx, "e:1").Result()
	if err != nil {
		t.Fatal(err)
	}
	back := dynamicpb.NewMessage(md)
	if err := UnmarshalEntity(hash, back); err != nil {
		t.Fatal(err)
	}
	if back.Has(ageFd) {
		t.Errorf("the cleared field reads back as %v", back.Get(ageFd))
	}
}
