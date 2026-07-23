package restgw

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// uint64BE folds a byte slice into a stable 64-bit fingerprint:
// short input zero-pads on the right; an 8+ byte input reads the
// leading 8 bytes big-endian. Pin both arms (the helper is the
// future cache-stat fingerprint and otherwise unexercised).
func TestUint64BE_PadsShortAndReadsLong(t *testing.T) {
	if got := uint64BE([]byte{0x01}); got != 0x0100000000000000 {
		t.Errorf("short pad: got %#x, want %#x", got, uint64(0x0100000000000000))
	}
	if got := uint64BE(nil); got != 0 {
		t.Errorf("nil input: got %#x, want 0", got)
	}
	long := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xff}
	if got := uint64BE(long); got != 1 {
		t.Errorf("long input reads first 8 BE bytes: got %#x, want 1", got)
	}
}

// withAuthUserData is a no-op for empty userData — the NoAuth
// path must not stamp a phantom principal on the context.
func TestWithAuthUserData_EmptyIsNoOp(t *testing.T) {
	ctx := context.Background()
	if got := withAuthUserData(ctx, nil); got != ctx {
		t.Error("nil userData should return the ctx unchanged")
	}
	if AuthUserDataFromContext(ctx) != nil {
		t.Error("no principal should be present")
	}
	withData := withAuthUserData(ctx, []byte{0x01, 0x02})
	if got := AuthUserDataFromContext(withData); string(got) != "\x01\x02" {
		t.Errorf("principal bytes = %v, want [1 2]", got)
	}
}

// reapExpired drops every entry past its TTL and keeps the rest.
// Drives the janitor's eviction body directly (the timer-driven
// caller fires once a minute, too slow for a unit test).
func TestReapExpired_DropsExpiredKeepsLive(t *testing.T) {
	s := NewMemoryTicketStore()
	defer s.Close()
	live, _ := s.Issue(context.Background(), map[string]string{"k": "live"}, time.Hour)
	dead, _ := s.Issue(context.Background(), map[string]string{"k": "dead"}, time.Hour)

	// Force `dead` into the past, then reap as of now.
	s.mu.Lock()
	e := s.entries[dead]
	e.expiresAt = time.Now().Add(-time.Minute)
	s.entries[dead] = e
	s.mu.Unlock()

	s.reapExpired(time.Now())

	s.mu.Lock()
	_, deadStillThere := s.entries[dead]
	_, liveStillThere := s.entries[live]
	s.mu.Unlock()
	if deadStillThere {
		t.Error("expired entry should be reaped")
	}
	if !liveStillThere {
		t.Error("live entry should survive the reap")
	}
}

// validateExtension rejects a nil multipart header (no filename to
// match) when the allowlist isn't the "*" wildcard. Defensive arm
// unreachable through ProcessFilePart (FormFile always yields a
// header), so it's pinned white-box.
func TestValidateExtension_NilHeaderRejected(t *testing.T) {
	if err := validateExtension(nil, []string{"pdf"}); err == nil {
		t.Error("nil header with a non-wildcard allowlist must be rejected")
	}
	if err := validateExtension(nil, []string{"*"}); err != nil {
		t.Errorf("wildcard short-circuits before the header check: %v", err)
	}
}

// wsFormatOpt defaults to JSON when no explicit format rides the
// variadic — keeps pre-C9 WSReadProto / WSWriteProto callers on the
// text/JSON wire.
func TestWsFormatOpt_DefaultsJSON(t *testing.T) {
	if got := wsFormatOpt(nil); got != WireJSON {
		t.Errorf("absent format = %v, want WireJSON", got)
	}
	if got := wsFormatOpt([]WireFormat{WireProto}); got != WireProto {
		t.Errorf("explicit format = %v, want WireProto", got)
	}
}

// --- alias rewrite at depth -------------------------------------

// richAliasDescriptor compiles a message whose alias annotations sit
// at every shape the rewrite must descend: a scalar field, a nested
// singular message, a repeated message, and a map<string, message>.
func richAliasDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	mapEntry := &descriptorpb.DescriptorProto{
		Name: proto.String("PropsEntry"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:   proto.String("key"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
			{
				Name:     proto.String("value"),
				Number:   proto.Int32(2),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".aliastest.Profile"),
			},
		},
		Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("rich_alias.proto"),
		Package:    proto.String("aliastest"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"w17/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Profile"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("full_name"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("fullName"),
						Options:  fieldOptionsWithAlias("name"),
					},
				},
			},
			{
				Name: proto.String("Tag"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("tag_id"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("tagId"),
						Options:  fieldOptionsWithAlias("tid"),
					},
				},
			},
			{
				Name:       proto.String("User"),
				NestedType: []*descriptorpb.DescriptorProto{mapEntry},
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("user_id"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("userId"),
						Options:  fieldOptionsWithAlias("id"),
					},
					{
						Name:     proto.String("profile"),
						Number:   proto.Int32(2),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".aliastest.Profile"),
						JsonName: proto.String("profile"),
					},
					{
						Name:     proto.String("tags"),
						Number:   proto.Int32(3),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".aliastest.Tag"),
						JsonName: proto.String("tags"),
					},
					{
						Name:     proto.String("props"),
						Number:   proto.Int32(4),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".aliastest.User.PropsEntry"),
						JsonName: proto.String("props"),
					},
				},
			},
		},
	}
	return buildMessageDescriptor(t, file, "aliastest.User")
}

// rewriteAliasesOnResponseJSON renames aliased keys at every nesting
// depth — scalar, singular message, repeated message, and map value
// — while leaving unknown keys and non-aliased message fields intact.
func TestRewriteAliases_AllShapes(t *testing.T) {
	desc := richAliasDescriptor(t)
	in := []byte(`{
		"user_id":"u1",
		"profile":{"full_name":"Alice"},
		"tags":[{"tag_id":"t1"},{"tag_id":"t2"}],
		"props":{"home":{"full_name":"Home"}},
		"unknown_field":42
	}`)
	out, err := rewriteAliasesOnResponseJSON(in, desc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["id"] != "u1" {
		t.Errorf("scalar alias missing: %v", obj)
	}
	if _, ok := obj["user_id"]; ok {
		t.Error("proto name should be gone after scalar rename")
	}
	prof := obj["profile"].(map[string]any)
	if prof["name"] != "Alice" {
		t.Errorf("nested message alias missing: %v", prof)
	}
	tags := obj["tags"].([]any)
	if tags[0].(map[string]any)["tid"] != "t1" {
		t.Errorf("repeated message alias missing: %v", tags)
	}
	props := obj["props"].(map[string]any)
	if props["home"].(map[string]any)["name"] != "Home" {
		t.Errorf("map-value message alias missing: %v", props)
	}
	if obj["unknown_field"] != float64(42) {
		t.Errorf("unknown key should pass through untouched: %v", obj)
	}
}

// restoreAliasesOnRequestJSON is the inbound mirror: alias keys at
// every depth rewrite back to proto names so protojson finds them;
// unknown keys pass through.
func TestRestoreAliases_AllShapes(t *testing.T) {
	desc := richAliasDescriptor(t)
	in := []byte(`{
		"id":"u1",
		"profile":{"name":"Alice"},
		"tags":[{"tid":"t1"}],
		"props":{"home":{"name":"Home"}},
		"mystery":1
	}`)
	out, err := restoreAliasesOnRequestJSON(in, desc)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["user_id"] != "u1" {
		t.Errorf("scalar alias not restored to proto name: %v", obj)
	}
	if _, ok := obj["id"]; ok {
		t.Error("alias key should be gone after restore")
	}
	if obj["profile"].(map[string]any)["full_name"] != "Alice" {
		t.Errorf("nested restore failed: %v", obj["profile"])
	}
	if obj["tags"].([]any)[0].(map[string]any)["tag_id"] != "t1" {
		t.Errorf("repeated restore failed: %v", obj["tags"])
	}
	if obj["props"].(map[string]any)["home"].(map[string]any)["full_name"] != "Home" {
		t.Errorf("map-value restore failed: %v", obj["props"])
	}
	if obj["mystery"] != float64(1) {
		t.Errorf("unknown key should pass through: %v", obj)
	}
}

// MarshalProto and UnmarshalProto run their alias post/pre-processing
// against a real (dynamicpb) message that carries rest_alias — the
// JSON surface shows the alias, and the alias round-trips back into
// the proto field on decode.
func TestMarshalUnmarshalProto_AliasRoundTrip(t *testing.T) {
	desc := richAliasDescriptor(t)
	msg := dynamicpb.NewMessage(desc)
	fd := desc.Fields().ByName("user_id")
	msg.Set(fd, protoreflect.ValueOfString("u-77"))

	out, err := MarshalProto(msg)
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["id"] != "u-77" {
		t.Errorf("MarshalProto should emit the alias key; got %v", obj)
	}

	// Decode the aliased wire back into a fresh message.
	got := dynamicpb.NewMessage(desc)
	if err := UnmarshalProto([]byte(`{"id":"u-99"}`), got); err != nil {
		t.Fatalf("UnmarshalProto: %v", err)
	}
	if v := got.Get(fd).String(); v != "u-99" {
		t.Errorf("alias key should decode into the proto field; got %q", v)
	}
}
