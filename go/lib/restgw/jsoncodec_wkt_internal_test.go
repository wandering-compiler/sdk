package restgw

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

func TestAppendStructJSON(t *testing.T) {
	if got := string(AppendStructJSON(nil, nil)); got != "null" {
		t.Errorf("nil = %q, want null", got)
	}
	s, _ := structpb.NewStruct(map[string]any{"a": "x", "n": 1.0})
	got := string(AppendStructJSON(nil, s))
	if !strings.Contains(got, `"a":"x"`) || !strings.Contains(got, `"n":1`) {
		t.Errorf("struct = %q", got)
	}
}

func TestAppendValueJSON(t *testing.T) {
	if got := string(AppendValueJSON(nil, nil)); got != "null" {
		t.Errorf("nil = %q, want null", got)
	}
	for _, c := range []struct {
		v    *structpb.Value
		want string
	}{
		{structpb.NewStringValue("hi"), `"hi"`},
		{structpb.NewNumberValue(42), `42`},
		{structpb.NewBoolValue(true), `true`},
		{structpb.NewNullValue(), `null`},
	} {
		if got := string(AppendValueJSON(nil, c.v)); got != c.want {
			t.Errorf("value = %q, want %q", got, c.want)
		}
	}
}

func TestAppendListValueJSON(t *testing.T) {
	if got := string(AppendListValueJSON(nil, nil)); got != "null" {
		t.Errorf("nil = %q, want null", got)
	}
	lv, _ := structpb.NewList([]any{"a", 2.0})
	if got := string(AppendListValueJSON(nil, lv)); got != `["a",2]` {
		t.Errorf("list = %q, want [\"a\",2]", got)
	}
}

func TestAppendFieldMaskJSON(t *testing.T) {
	if got := string(AppendFieldMaskJSON(nil, nil)); got != "null" {
		t.Errorf("nil = %q, want null", got)
	}
	fm := &fieldmaskpb.FieldMask{Paths: []string{"user_id", "created_at.unix_nanos"}}
	if got := string(AppendFieldMaskJSON(nil, fm)); got != `"userId,createdAt.unixNanos"` {
		t.Errorf("fieldmask = %q", got)
	}
}

func TestAppendAnyJSON(t *testing.T) {
	// nil → null.
	if got, err := AppendAnyJSON(nil, nil); err != nil || string(got) != "null" {
		t.Errorf("nil = (%q,%v)", got, err)
	}
	// unresolvable type URL → error.
	if _, err := AppendAnyJSON(nil, &anypb.Any{TypeUrl: "type.googleapis.com/nope.Nope"}); err == nil {
		t.Error("unresolvable type URL must error")
	}
	// regular message with fields → @type spliced into the object.
	withFields, _ := anypb.New(&w17pb.ErrorDetail{Field: "email", Code: "UNIQUE"})
	got, err := AppendAnyJSON(nil, withFields)
	if err != nil {
		t.Fatalf("regular any: %v", err)
	}
	gs := string(got)
	if !strings.HasPrefix(gs, `{"@type":`) || !strings.Contains(gs, "email") {
		t.Errorf("regular any = %q", gs)
	}
	// custom WKT (Timestamp) → wrapped under "value".
	ts, _ := anypb.New(timestamppb.New(time.Unix(0, 0).UTC()))
	got2, err := AppendAnyJSON(nil, ts)
	if err != nil {
		t.Fatalf("ts any: %v", err)
	}
	if !strings.Contains(string(got2), `"value":`) {
		t.Errorf("custom-WKT any must use the value envelope, got %q", got2)
	}
	// resolvable type URL but corrupt value bytes → unmarshal error.
	bad := &anypb.Any{TypeUrl: withFields.GetTypeUrl(), Value: []byte{0x08}}
	if _, err := AppendAnyJSON(nil, bad); err == nil {
		t.Error("corrupt Any value must surface an unmarshal error")
	}
}

func TestIsCustomJSONWKT(t *testing.T) {
	if !isCustomJSONWKT("google.protobuf.Timestamp") || !isCustomJSONWKT("google.protobuf.StringValue") {
		t.Error("WKTs must be custom")
	}
	if isCustomJSONWKT("acme.app.Task") {
		t.Error("a normal message must not be custom")
	}
}
