package wire

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	_ "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The fixture is the converter's own output for its feature document (the
// generator's golden proto); a generator-side test keeps this copy identical, so the codec is tested against exactly
// what the generator emits.
var (
	shop   = compileFixture("feature_shop")
	shapes = compileFixture("shapes")
	probe  = compileFixture("probe")
)

func compileFixture(name string) protoreflect.FileDescriptor {
	src, err := os.ReadFile("testdata/" + name + ".proto")
	if err != nil {
		panic(err)
	}
	path := "clients/" + name + "/" + name + ".proto"
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(protocompile.ResolverFunc(func(p string) (protocompile.SearchResult, error) {
		if p == path {
			return protocompile.SearchResult{Source: strings.NewReader(string(src))}, nil
		}
		fd, err := protoregistry.GlobalFiles.FindFileByPath(p)
		if err != nil {
			return protocompile.SearchResult{}, err
		}
		return protocompile.SearchResult{Desc: fd}, nil
	}))}
	files, err := c.Compile(context.Background(), path)
	if err != nil {
		panic(err)
	}
	return files[0]
}

func md(t *testing.T, name string) protoreflect.MessageDescriptor {
	t.Helper()
	return mdIn(t, shop, name)
}

func mdIn(t *testing.T, fd protoreflect.FileDescriptor, name string) protoreflect.MessageDescriptor {
	t.Helper()
	parts := strings.Split(name, ".")
	m := fd.Messages().ByName(protoreflect.Name(parts[0]))
	for _, p := range parts[1:] {
		m = m.Messages().ByName(protoreflect.Name(p))
	}
	if m == nil {
		t.Fatalf("no message %s", name)
	}
	return m
}

// fromProtoJSON builds a message from its proto3 JSON form — the readable
// way to write one down in a test.
func fromProtoJSON(t *testing.T, name, js string) *dynamicpb.Message {
	t.Helper()
	m := dynamicpb.NewMessage(md(t, name))
	if err := protojson.Unmarshal([]byte(js), m); err != nil {
		t.Fatalf("protojson %s: %v", name, err)
	}
	return m
}

const orderProtoJSON = `{
  "id": "o1",
  "status": "ORDER_STATUS_REQUIRES_PAYMENT",
  "customer": {"string_value": "cus_1"},
  "payment_method": {"card": {"last4": "4242", "exp_month": 12}},
  "pet": {"cat": {"name": "Tom", "indoor": true}},
  "lines": [{"sku": "s1", "quantity": "2", "unit_price": 9.5}],
  "matrix": [{"value": [1, 2]}, {"value": [3]}],
  "attachments_by_kind": {"pdf": {"value": ["a.pdf"]}},
  "channel": "CHANNEL_WEB",
  "anything": {"x": 1},
  "reference": {"integer_value": "42"}
}`

// What the upstream reads and writes for that order: its keys, its enum
// strings, numbers for 64-bit integers, each union as the value of its arm
// with the discriminator the arm decides, wrappers as their arrays.
const orderWire = `{"id":"o1","status":"requires_payment","customer":"cus_1",` +
	`"paymentMethod":{"object":"card","last4":"4242","expMonth":12},` +
	`"pet":{"petType":"Cat","name":"Tom","indoor":true},` +
	`"lines":[{"sku":"s1","quantity":2,"unitPrice":9.5}],` +
	`"matrix":[[1,2],[3]],"attachmentsByKind":{"pdf":["a.pdf"]},` +
	`"channel":"web","anything":{"x":1},"reference":42}`

func TestMarshal_Order(t *testing.T) {
	got, err := Marshal(fromProtoJSON(t, "Order", orderProtoJSON))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orderWire {
		t.Errorf("got\n%s\nwant\n%s", got, orderWire)
	}
}

func TestUnmarshal_Order(t *testing.T) {
	m := dynamicpb.NewMessage(md(t, "Order"))
	if err := Unmarshal([]byte(orderWire), m); err != nil {
		t.Fatal(err)
	}
	want := fromProtoJSON(t, "Order", orderProtoJSON)
	// The discriminators the encoder wrote come back as the arm messages'
	// own fields — they are properties of Card and Cat in the document.
	want.Mutable(md(t, "Order").Fields().ByName("payment_method")).Message().Mutable(md(t, "PaymentMethod").Fields().ByName("card")).Message().
		Set(md(t, "Card").Fields().ByName("object"), protoreflect.ValueOfString("card"))
	want.Mutable(md(t, "Order").Fields().ByName("pet")).Message().Mutable(md(t, "Pet").Fields().ByName("cat")).Message().
		Set(md(t, "Cat").Fields().ByName("pet_type"), protoreflect.ValueOfString("Cat"))
	if !proto.Equal(m, want) {
		g, _ := protojson.Marshal(m)
		w, _ := protojson.Marshal(want)
		t.Errorf("got\n%s\nwant\n%s", g, w)
	}
}

// The untagged customer union takes a string id or the expanded object.
func TestUnmarshal_UntaggedUnionByShape(t *testing.T) {
	m := dynamicpb.NewMessage(md(t, "Order"))
	if err := Unmarshal([]byte(`{"id":"o","customer":{"id":"cus_1","email":"a@b.c","nickname":null}}`), m); err != nil {
		t.Fatal(err)
	}
	c := m.Get(md(t, "Order").Fields().ByName("customer")).Message()
	arm := c.WhichOneof(c.Descriptor().Oneofs().ByName("value"))
	if arm == nil || arm.Name() != "customer" {
		t.Fatalf("customer arm = %v", arm)
	}
	cust := c.Get(arm).Message()
	if !cust.Has(md(t, "Customer").Fields().ByName("email")) || cust.Has(md(t, "Customer").Fields().ByName("nickname")) {
		t.Error("a null leaves an optional field unset; a value sets it")
	}
}

func TestUnmarshal_Lenience(t *testing.T) {
	var issues []Issue
	opts := UnmarshalOptions{Report: func(i Issue) { issues = append(issues, i) }}
	m := dynamicpb.NewMessage(md(t, "Order"))
	err := opts.Unmarshal([]byte(`{
	  "id": "o", "status": "on_hold", "brandNewField": {"x": [1]},
	  "paymentMethod": {"object": "crypto", "wallet": "w"},
	  "lines": [{"sku": "s", "quantity": "7"}]
	}`), m)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get(md(t, "Order").Fields().ByName("status")).Enum(); got != 0 {
		t.Errorf("an unknown enum string decodes to the zero value, got %d", got)
	}
	pm := m.Get(md(t, "Order").Fields().ByName("payment_method")).Message()
	if pm.WhichOneof(pm.Descriptor().Oneofs().ByName("value")) != nil {
		t.Error("an unknown union tag leaves the union unset")
	}
	if q := m.Get(md(t, "Order").Fields().ByName("lines")).List().Get(0).Message().Get(md(t, "Order.LinesItem").Fields().ByName("quantity")).Int(); q != 7 {
		t.Errorf("a quoted integer is read as the integer, got %d", q)
	}
	var msgs []string
	for _, i := range issues {
		msgs = append(msgs, i.Path+" "+i.Message)
	}
	if len(issues) != 2 || !strings.Contains(msgs[0]+msgs[1], `"on_hold"`) || !strings.Contains(msgs[0]+msgs[1], `"crypto"`) {
		t.Errorf("issues = %v", msgs)
	}
}

func TestUnmarshal_Refusals(t *testing.T) {
	for name, tc := range map[string]struct{ msg, json, want string }{
		"wrong kind":          {"Order", `{"id": 5}`, "/id: expected string, got number"},
		"wrong kind deep":     {"Order", `{"id":"o","lines":[{"sku":"s","quantity":"many"}]}`, "/lines/0/quantity"},
		"not an object":       {"Order", `[1]`, "expected an object"},
		"fractional integer":  {"Order", `{"lines":[{"quantity":1.5}]}`, "expected an integer"},
		"32-bit overflow":     {"Card", `{"expMonth": 4294967296}`, "does not fit 32 bits"},
		"null in an array":    {"Order", `{"lines":[null]}`, "null in an array"},
		"union kind mismatch": {"Order", `{"customer": true}`, "no variant of the union takes a JSON boolean"},
		"trailing data":       {"Order", `{} {}`, "trailing data"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Unmarshal([]byte(tc.json), dynamicpb.NewMessage(md(t, tc.msg)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestMarshal_PresenceAndRefusals(t *testing.T) {
	// A required scalar is written at its zero value; an unset optional one
	// is not written at all.
	got, err := Marshal(fromProtoJSON(t, "Card", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"object":"","last4":""}` {
		t.Errorf("got %s", got)
	}
	// A required enum at _UNSPECIFIED has no upstream string to send.
	if _, err := Marshal(fromProtoJSON(t, "Order", `{"id":"o"}`)); err == nil || !strings.Contains(err.Error(), "/status") {
		t.Errorf("err = %v", err)
	}
}

func TestWrapperBodies(t *testing.T) {
	got, err := Marshal(fromProtoJSON(t, "ListOrdersResponse", `{"value": [{"id": "a", "status": "ORDER_STATUS_PAID", "customer": {"string_value": "c"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[{"id":"a","status":"paid","customer":"c"}]` {
		t.Errorf("an array body is the wrapper's array: %s", got)
	}
	m := dynamicpb.NewMessage(md(t, "OrderTotalResponse"))
	if err := Unmarshal([]byte(`12.5`), m); err != nil {
		t.Fatal(err)
	}
	if v := m.Get(md(t, "OrderTotalResponse").Fields().ByName("value")).Float(); v != 12.5 {
		t.Errorf("a scalar body = %v", v)
	}
}

func TestForm(t *testing.T) {
	body := fromProtoJSON(t, "CreateOrderRequest.Body", `{"customer": "cus_1", "metadata": {"order_id": "42", "a b": "c&d"}}`)
	vals, err := Form(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := vals.Encode(); got != "customer=cus_1&metadata%5Ba+b%5D=c%26d&metadata%5Border_id%5D=42" {
		t.Errorf("form = %s", got)
	}
	order := fromProtoJSON(t, "Order", orderProtoJSON)
	// `anything` is a google.protobuf.Value, which has no form encoding.
	order.Clear(md(t, "Order").Fields().ByName("anything"))
	vals, err = Form(order)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"paymentMethod[object]": "card", "paymentMethod[last4]": "4242", "pet[petType]": "Cat",
		"lines[0][quantity]": "2", "lines[0][unitPrice]": "9.5", "matrix[1][0]": "3",
		"attachmentsByKind[pdf][0]": "a.pdf", "status": "requires_payment", "customer": "cus_1", "reference": "42",
	} {
		if got := vals.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	vals, err = Form(fromProtoJSON(t, "Customer", `{"id": "c", "email": "e", "preferences": {"a": 1, "b": [true, "x"], "n": null}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := vals.Encode(); got != "email=e&id=c&preferences%5Ba%5D=1&preferences%5Bb%5D%5B0%5D=true&preferences%5Bb%5D%5B1%5D=x" {
		t.Errorf("a Struct is written as a deep object, null left out: %s", got)
	}
}

func drawing(t *testing.T, js string) (*dynamicpb.Message, error) {
	t.Helper()
	m := dynamicpb.NewMessage(mdIn(t, shapes, "Drawing"))
	return m, Unmarshal([]byte(js), m)
}

func armOf(m protoreflect.Message, field string) string {
	u := m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(field))).Message()
	arm := u.WhichOneof(u.Descriptor().Oneofs().ByName("value"))
	if arm == nil {
		return ""
	}
	return string(arm.Name())
}

// An untagged union of objects is resolved by required properties first,
// then by the fewest properties foreign to the arm; an open arm (a
// free-form object) takes what no declared shape does.
func TestUnmarshal_ObjectArmsByShape(t *testing.T) {
	for js, want := range map[string]string{
		`{"shape": {"radius": 1}}`:                    "circle",
		`{"shape": {"w": 1, "h": 2, "label": "x"}}`:   "rect",
		`{"shape": {"w": 1, "h": 2, "radius": 3}}`:    "rect", // both complete; Circle has two foreign keys, Rect one
		`{"shape": {"w": 1}}`:                         "object_value",
		`{"shape": {"anything": "else"}}`:             "object_value",
		`{"shape": {"radius": 1, "label": "circle"}}`: "circle",
	} {
		m, err := drawing(t, js)
		if err != nil {
			t.Errorf("%s: %v", js, err)
			continue
		}
		if got := armOf(m, "shape"); got != want {
			t.Errorf("%s: arm %q, want %q", js, got, want)
		}
	}
}

func TestUnmarshal_ScalarArmsByKind(t *testing.T) {
	for js, want := range map[string]string{
		`{"tag": "s"}`:        "string_value",
		`{"tag": 5}`:          "integer_value",
		`{"tag": true}`:       "boolean_value",
		`{"tag": ["a", "b"]}`: "array_value",
	} {
		m, err := drawing(t, js)
		if err != nil {
			t.Errorf("%s: %v", js, err)
			continue
		}
		if got := armOf(m, "tag"); got != want {
			t.Errorf("%s: arm %q, want %q", js, got, want)
		}
	}
	if _, err := drawing(t, `{"tag": {"k": 1}}`); err == nil || !strings.Contains(err.Error(), "/tag: no variant of the union takes a JSON object") {
		t.Errorf("err = %v", err)
	}
}

func TestBytesAndTimestamps_RoundTrip(t *testing.T) {
	const in = `{"thumbnail":"aGk/Pz4+","drawnAt":"2026-10-07T12:00:00Z","size":3}`
	m, err := drawing(t, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(m.Get(mdIn(t, shapes, "Drawing").Fields().ByName("thumbnail")).Bytes()); got != "hi??>>" {
		t.Errorf("thumbnail = %q", got)
	}
	out, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("round trip:\n got %s\nwant %s", out, in)
	}
	// URL-safe and unpadded base64 are read too: upstreams use all four.
	if _, err := drawing(t, `{"thumbnail":"aGk_Pz4-"}`); err != nil {
		t.Errorf("url-safe base64: %v", err)
	}
	if _, err := drawing(t, `{"thumbnail":"!!"}`); err == nil || !strings.Contains(err.Error(), "not base64") {
		t.Errorf("err = %v", err)
	}
	if _, err := drawing(t, `{"drawnAt":"yesterday"}`); err == nil || !strings.Contains(err.Error(), "/drawnAt") {
		t.Errorf("err = %v", err)
	}
}

func TestMarshal_ObjectArm(t *testing.T) {
	m := dynamicpb.NewMessage(mdIn(t, shapes, "Drawing"))
	if err := protojson.Unmarshal([]byte(`{"shape": {"object_value": {"z": 1}}, "tag": {"array_value": {"value": ["a"]}}}`), m); err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"shape":{"z":1},"tag":["a"]}` {
		t.Errorf("got %s", out)
	}
}
