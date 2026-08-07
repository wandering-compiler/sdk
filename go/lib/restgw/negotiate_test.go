package restgw_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// RequestFormat reads Content-Type: anything but a protobuf media
// type (including empty / json / form) decodes as JSON.
func TestRequestFormat(t *testing.T) {
	cases := []struct {
		contentType string
		want        restgw.WireFormat
	}{
		{"", restgw.WireJSON},
		{"application/json", restgw.WireJSON},
		{"application/json; charset=utf-8", restgw.WireJSON},
		{"application/x-www-form-urlencoded", restgw.WireJSON},
		{"application/protobuf", restgw.WireProto},
		{"application/x-protobuf", restgw.WireProto},
		{"application/protobuf; charset=binary", restgw.WireProto},
		{"  APPLICATION/PROTOBUF  ", restgw.WireProto},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		if c.contentType != "" {
			req.Header.Set("Content-Type", c.contentType)
		}
		if got := restgw.RequestFormat(req); got != c.want {
			t.Errorf("RequestFormat(%q) = %v, want %v", c.contentType, got, c.want)
		}
	}
}

// ResponseFormat reads Accept, honouring the client's `;q=`
// weights: the highest-weighted recognised media type wins, ties
// break toward specificity then list order; absent / unrecognised
// defaults to JSON.
func TestResponseFormat(t *testing.T) {
	cases := []struct {
		accept string
		want   restgw.WireFormat
	}{
		{"", restgw.WireJSON},
		{"*/*", restgw.WireJSON},
		{"application/json", restgw.WireJSON},
		{"application/protobuf", restgw.WireProto},
		{"application/x-protobuf", restgw.WireProto},
		{"application/json, application/protobuf", restgw.WireJSON},
		{"application/protobuf, application/json", restgw.WireProto},
		{"text/html, application/protobuf;q=0.9", restgw.WireProto},

		// R-restgw-4 — q-weights are honoured, not just list order.
		// A higher-weighted type wins even when listed second.
		{"application/protobuf;q=0.5, application/json;q=0.9", restgw.WireJSON},
		{"application/json;q=0.2, application/protobuf;q=0.8", restgw.WireProto},
		{"application/protobuf;q=0.9, application/json;q=0.9", restgw.WireProto}, // tie → list order (proto first)
		// q=0 marks a type explicitly unacceptable → drops out.
		{"application/protobuf, application/json;q=0", restgw.WireProto},
		{"application/json, application/protobuf;q=0", restgw.WireJSON},
		{"application/protobuf;q=0, application/json", restgw.WireJSON},
		// Specificity tiebreak at equal q: exact type beats `*/*`.
		{"application/protobuf, */*", restgw.WireProto},
		// But an explicit lower q loses to a higher-q wildcard.
		{"application/protobuf;q=0.9, */*", restgw.WireJSON},
		// Whitespace + parameter ordering around q is tolerated.
		{"application/protobuf ; q=0.1 , application/json ; q=0.2", restgw.WireJSON},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if c.accept != "" {
			req.Header.Set("Accept", c.accept)
		}
		if got := restgw.ResponseFormat(req); got != c.want {
			t.Errorf("ResponseFormat(%q) = %v, want %v", c.accept, got, c.want)
		}
	}
}

// WriteResponse under a protobuf Accept emits a binary body with
// the protobuf media type that round-trips via proto.Unmarshal.
func TestWriteResponse_Protobuf(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept", "application/protobuf")

	restgw.WriteResponse(rec, http.StatusOK, wrapperspb.String("hello"), req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/protobuf" {
		t.Fatalf("Content-Type = %q, want application/protobuf", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	// JSON path would have produced quotes/braces; binary proto
	// of StringValue("hello") is the tag+len+ascii bytes.
	var got wrapperspb.StringValue
	if err := proto.Unmarshal(body, &got); err != nil {
		t.Fatalf("response body is not valid protobuf: %v", err)
	}
	if got.GetValue() != "hello" {
		t.Errorf("decoded value = %q, want hello", got.GetValue())
	}
}

// DecodeRequest under a protobuf Content-Type reads a binary body
// via proto.Unmarshal.
// restgw-sec-2 regression: an over-limit body is rejected, not read into
// memory unbounded.
func TestDecodeRequest_BodyTooLarge(t *testing.T) {
	orig := restgw.MaxRequestBodyBytes
	restgw.MaxRequestBodyBytes = 16
	defer func() { restgw.MaxRequestBodyBytes = orig }()

	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(make([]byte, 1024)))
	req.Header.Set("Content-Type", "application/json")
	var msg wrapperspb.StringValue
	err := restgw.DecodeRequest(req, &msg)
	if err == nil {
		t.Fatal("expected an error for an over-limit body")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should cite the size limit, got: %v", err)
	}
}

func TestDecodeRequest_Protobuf(t *testing.T) {
	raw, err := proto.Marshal(wrapperspb.String("world"))
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/protobuf")

	var msg wrapperspb.StringValue
	if err := restgw.DecodeRequest(req, &msg); err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if msg.GetValue() != "world" {
		t.Errorf("decoded value = %q, want world", msg.GetValue())
	}
}

// DecodeRequestRewrite hands the RAW JSON body to a caller-supplied key
// rewrite before decoding — the admin login form's "username" against a
// login_method that declares a different identifier field.
func TestDecodeRequestRewrite(t *testing.T) {
	rename := func(body []byte) []byte {
		return bytes.ReplaceAll(body, []byte(`"username"`), []byte(`"email"`))
	}

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"username":"ada@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	var msg structpb.Struct
	if err := restgw.DecodeRequestRewrite(req, &msg, rename); err != nil {
		t.Fatalf("DecodeRequestRewrite: %v", err)
	}
	if got := msg.GetFields()["email"].GetStringValue(); got != "ada@example.com" {
		t.Errorf("rewritten key did not reach the message: fields=%v", msg.GetFields())
	}

	// A protobuf body keys on field numbers — there is no name to
	// rewrite, so the hook must NOT run (running it would corrupt the
	// binary payload).
	fixture, err := structpb.NewStruct(map[string]any{"email": "bin@example.com"})
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	raw, err := proto.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	called := false
	req = httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/protobuf")
	msg.Reset()
	if err := restgw.DecodeRequestRewrite(req, &msg, func(b []byte) []byte { called = true; return b }); err != nil {
		t.Fatalf("DecodeRequestRewrite protobuf: %v", err)
	}
	if called {
		t.Error("the JSON rewrite ran on a protobuf body")
	}
	if got := msg.GetFields()["email"].GetStringValue(); got != "bin@example.com" {
		t.Errorf("protobuf decode = %q, want bin@example.com", got)
	}

	// An empty body stays a no-op: nothing to rewrite, nothing to decode.
	called = false
	req = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	msg.Reset()
	if err := restgw.DecodeRequestRewrite(req, &msg, func(b []byte) []byte { called = true; return b }); err != nil {
		t.Fatalf("DecodeRequestRewrite empty: %v", err)
	}
	if called || len(msg.GetFields()) != 0 {
		t.Errorf("empty body should not invoke the rewrite (called=%v, fields=%v)", called, msg.GetFields())
	}
}
