package redis_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// userFDS hand-builds a FileDescriptorSet for a synthetic
// `test.User { string id, string full_name }` message and returns
// (base64 of the FDS bytes, the resolved descriptor). The base64 is
// what a migration's `proto_descriptor` YAML field carries.
func userFDS(t *testing.T) (string, protoreflect.MessageDescriptor) {
	t.Helper()
	stringT := descriptorpb.FieldDescriptorProto_TYPE_STRING
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	syntax := "proto3"
	pkg := "test"
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("user.proto"),
		Package: &pkg,
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("User"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Type: &stringT, Label: &optional},
				{Name: proto.String("full_name"), Number: proto.Int32(2), Type: &stringT, Label: &optional},
			},
		}},
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	raw, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal FDS: %v", err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("test.User")
	if err != nil {
		t.Fatalf("find User: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw), d.(protoreflect.MessageDescriptor)
}

// TestApply_YAMLProtobufCodec_Live pins the protobuf-encoded data
// migration through the ProtoCodec apply branch (the per-key codec
// path the JSON tests never reach): a proto-encoded value is GET, the
// codec sets the missing scalar default, and the mutated proto is SET
// back — observable by decoding the stored bytes.
func TestApply_YAMLProtobufCodec_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	b64, desc := userFDS(t)

	// Store a User with only id set (full_name absent).
	msg := dynamicpb.NewMessage(desc)
	msg.Set(desc.Fields().ByName("id"), protoreflect.ValueOfString("u1"))
	rawBytes, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal User: %v", err)
	}
	_ = mr.Set("users:1", string(rawBytes))

	body := fmt.Sprintf(`version: 1
encoding: protobuf
proto_descriptor: %s
proto_message: test.User
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: users:*
    field: full_name
    value: anonymous
`, b64)
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "20260601T000000Z", UpSql: body}); err != nil {
		t.Fatalf("Apply protobuf YAML: %v", err)
	}

	got, _ := mr.Get("users:1")
	out := dynamicpb.NewMessage(desc)
	if err := proto.Unmarshal([]byte(got), out); err != nil {
		t.Fatalf("decode migrated User: %v", err)
	}
	if fn := out.Get(desc.Fields().ByName("full_name")).String(); fn != "anonymous" {
		t.Errorf("full_name = %q, want anonymous (codec apply branch)", fn)
	}
}

// TestApply_YAMLGetWrongType_Live pins applyOpToKey's non-Nil GET
// error arm: a key whose type is not a string (here a hash) is matched
// by SCAN but fails GET with WRONGTYPE, surfacing as a per-key error
// rather than a silent skip.
func TestApply_YAMLGetWrongType_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	mr.HSet("users:1", "f", "v") // a hash, not a string

	err := a.Apply(ctx, &applyfetchpb.Migration{Id: "20260601T000001Z", UpSql: yamlAddActive})
	if err == nil || !strings.Contains(err.Error(), "GET") {
		t.Fatalf("Apply over wrong-typed key err = %v, want GET error", err)
	}
}

// TestApply_YAMLBookkeepingWrongType_Live pins the forward
// recordMigrationApplied error arm: an HSET against a `wc:migrations`
// key that is the wrong type (a string, not a hash) surfaces as a
// bookkeeping error after the ops complete.
func TestApply_YAMLBookkeepingWrongType_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("wc:migrations", "not-a-hash") // poison the bookkeeping key

	err := a.Apply(ctx, &applyfetchpb.Migration{Id: "20260601T000002Z", UpSql: yamlAddActive})
	if err == nil || !strings.Contains(err.Error(), "bookkeeping") {
		t.Fatalf("Apply with poisoned bookkeeping key err = %v, want bookkeeping error", err)
	}
}

// TestRollback_YAMLBookkeepingWrongType_Live pins the rollback
// recordMigrationRolledBack error arm: an HDEL against a wrong-typed
// `wc:migrations` key surfaces as a bookkeeping error.
func TestRollback_YAMLBookkeepingWrongType_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("wc:migrations", "not-a-hash")
	down := `version: 1
encoding: json
operations:
  - op: REMOVE_FIELD
    keyspace: users:*
    field: active
`
	err := a.Rollback(ctx, &applyfetchpb.Migration{Id: "20260601T000003Z", UpSql: yamlAddActive, DownSql: down})
	if err == nil || !strings.Contains(err.Error(), "bookkeeping") {
		t.Fatalf("Rollback with poisoned bookkeeping key err = %v, want bookkeeping error", err)
	}
}

// TestRollback_YAMLProtobufCodecError pins the rollback buildCodec
// refusal arm: a protobuf down body with a non-FDS descriptor is
// rejected before any network call (mirrors the forward codec refusal).
func TestRollback_YAMLProtobufCodecError(t *testing.T) {
	down := `version: 1
encoding: protobuf
proto_descriptor: aGVsbG8=
proto_message: pkg.User
operations:
  - op: REMOVE_FIELD
    keyspace: x:*
    field: y
`
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id: "ts-rbpb", UpSql: yamlAddBody, DownSql: down,
	})
	if err == nil || strings.Contains(err.Error(), "cursor") {
		t.Errorf("expected rollback codec error before network, got %v", err)
	}
}

// TestRollback_YAMLTransformCompileError pins the rollback
// buildTransformVMs refusal arm: a down-body TRANSFORM_FIELD with an
// invalid script aborts before any network call.
func TestRollback_YAMLTransformCompileError(t *testing.T) {
	down := `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: x:*
    script_lang: starlark
    script: |
      x = 1
`
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id: "ts-rbtf", UpSql: yamlAddBody, DownSql: down,
	})
	if err == nil || strings.Contains(err.Error(), "cursor") {
		t.Errorf("expected rollback transform compile error before network, got %v", err)
	}
}

// TestRollback_YAMLGetWrongType_Live pins the rollback runDataOp error
// arm: a wrong-typed key matched by the down-direction SCAN fails GET and
// surfaces as an op-tagged rollback error.
func TestRollback_YAMLGetWrongType_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	mr.HSet("users:1", "f", "v") // a hash, not a string
	down := `version: 1
encoding: json
operations:
  - op: REMOVE_FIELD
    keyspace: users:*
    field: active
`
	err := a.Rollback(ctx, &applyfetchpb.Migration{Id: "20260601T000005Z", UpSql: yamlAddActive, DownSql: down})
	if err == nil || !strings.Contains(err.Error(), "REMOVE_FIELD") {
		t.Fatalf("Rollback over wrong-typed key err = %v, want op-tagged error", err)
	}
}

// TestRollback_YAMLResumesFromCursor_Live pins the rollback
// op-already-complete skip: a rollback cursor marking op 0 complete
// makes the rollback skip it, so the key keeps its field (no inverse
// re-applied) yet the run still finishes cleanly.
func TestRollback_YAMLResumesFromCursor_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("users:1", `{"name":"a","active":true}`)
	id := "20260601T000004Z"
	mr.HSet("wc:data-rollbacks:"+id, "0", "complete") // op 0 already done
	mr.HSet("wc:migrations", id, "feed")

	down := `version: 1
encoding: json
operations:
  - op: REMOVE_FIELD
    keyspace: users:*
    field: active
`
	if err := a.Rollback(ctx, &applyfetchpb.Migration{Id: id, UpSql: yamlAddActive, DownSql: down}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got, _ := mr.Get("users:1"); !strings.Contains(got, "active") {
		t.Errorf("op 0 should have been skipped (active must remain): %q", got)
	}
}
