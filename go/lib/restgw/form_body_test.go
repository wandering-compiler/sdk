package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func formRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/cb", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	return r
}

// An HTML form's POST — an OAuth provider with response_mode form_post
// comes back so — decodes into the request as the JSON object of its
// fields: a string, an integer and an enum name each from its value, and a
// field the message does not declare dropped as an unknown JSON key is.
func TestDecodeRequest_FormBody(t *testing.T) {
	var msg descriptorpb.FieldDescriptorProto
	err := restgw.DecodeRequest(formRequest("name=code&number=7&label=LABEL_REPEATED&user=%7B%22name%22%3A%7B%7D%7D"), &msg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.GetName() != "code" || msg.GetNumber() != 7 || msg.GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		t.Errorf("decoded %v", &msg)
	}
}

// A field given twice is a list; a single one a string.
func TestDecodeRequest_FormBodyRepeatedKey(t *testing.T) {
	var msg structpb.Struct
	if err := restgw.DecodeRequest(formRequest("a=1&b=x&b=y"), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := msg.GetFields()["a"].GetStringValue(); got != "1" {
		t.Errorf("a = %q", got)
	}
	if l := msg.GetFields()["b"].GetListValue().GetValues(); len(l) != 2 || l[0].GetStringValue() != "x" || l[1].GetStringValue() != "y" {
		t.Errorf("b = %v", msg.GetFields()["b"])
	}
}

// A body that is not a form is refused, never decoded as something else.
func TestDecodeRequest_FormBodyMalformed(t *testing.T) {
	var msg structpb.Struct
	if err := restgw.DecodeRequest(formRequest("a=%zz"), &msg); err == nil || !strings.Contains(err.Error(), "invalid form body") {
		t.Fatalf("a malformed form: %v", err)
	}
}
