package restgw

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestMarshalProtoAppend_NilMessage pins the msg==nil arm: the helper
// delegates to the configured Marshaller and the caller's existing dst
// prefix is preserved (whether protojson succeeds or errors, dst is the
// base of the returned slice).
func TestMarshalProtoAppend_NilMessage(t *testing.T) {
	prefix := []byte("PREFIX:")
	out, _ := MarshalProtoAppend(prefix, nil)
	if !strings.HasPrefix(string(out), "PREFIX:") {
		t.Errorf("nil-message append must preserve the dst prefix; got %q", out)
	}
}

// aliasedInnerMsg returns a populated instance of the aliased Inner
// message reused from the alias round-trip fixture (secret_field ⇄
// "secret"), so hasAnyAlias is true for its descriptor.
func aliasedInnerMsg(t *testing.T) proto.Message {
	t.Helper()
	innerDesc := aliasNestedDesc(t).Fields().ByName("one").Message()
	m := dynamicpb.NewMessage(innerDesc)
	m.Set(innerDesc.Fields().ByName("secret_field"), protoreflect.ValueOfString("x"))
	return m
}

// TestMarshalProtoAppend_AliasedRegisteredMarshaler — when a generated
// marshaler is registered for a descriptor that ALSO carries a rest_alias,
// the helper takes the rare "marshal in isolation, rewrite, append" arm:
// the marshaler emits the proto-name key, then the alias rewrite renames it.
func TestMarshalProtoAppend_AliasedRegisteredMarshaler(t *testing.T) {
	m := aliasedInnerMsg(t)
	RegisterJSONMarshaler(m.ProtoReflect().Descriptor().FullName(),
		func(dst []byte, _ proto.Message) ([]byte, error) {
			return append(dst, `{"secret_field":"x"}`...), nil
		})
	defer delete(jsonMarshalers, m.ProtoReflect().Descriptor().FullName())

	out, err := MarshalProtoAppend([]byte("P:"), m)
	if err != nil {
		t.Fatalf("MarshalProtoAppend: %v", err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "P:") {
		t.Errorf("dst prefix lost: %q", got)
	}
	if !strings.Contains(got, `"secret"`) || strings.Contains(got, `"secret_field"`) {
		t.Errorf("aliased+registered output must rename secret_field→secret; got %q", got)
	}
}

// TestMarshalProtoAppend_AliasedRegisteredMarshalerError — the registered
// marshaler's own error is returned, and the dst is left unchanged.
func TestMarshalProtoAppend_AliasedRegisteredMarshalerError(t *testing.T) {
	m := aliasedInnerMsg(t)
	boom := errors.New("boom")
	RegisterJSONMarshaler(m.ProtoReflect().Descriptor().FullName(),
		func(dst []byte, _ proto.Message) ([]byte, error) { return dst, boom })
	defer delete(jsonMarshalers, m.ProtoReflect().Descriptor().FullName())

	out, err := MarshalProtoAppend([]byte("P:"), m)
	if !errors.Is(err, boom) {
		t.Fatalf("expected marshaler error, got %v", err)
	}
	if string(out) != "P:" {
		t.Errorf("dst must be returned unchanged on marshaler error; got %q", out)
	}
}

// TestMarshalProtoAppend_AliasedRegisteredMarshalerBadJSON — when the
// registered marshaler emits invalid JSON, the subsequent alias rewrite's
// json.Unmarshal failure surfaces as the error.
func TestMarshalProtoAppend_AliasedRegisteredMarshalerBadJSON(t *testing.T) {
	m := aliasedInnerMsg(t)
	RegisterJSONMarshaler(m.ProtoReflect().Descriptor().FullName(),
		func(dst []byte, _ proto.Message) ([]byte, error) {
			return append(dst, `{not json`...), nil
		})
	defer delete(jsonMarshalers, m.ProtoReflect().Descriptor().FullName())

	if _, err := MarshalProtoAppend(nil, m); err == nil {
		t.Fatal("invalid marshaler output must fail the alias rewrite")
	}
}
