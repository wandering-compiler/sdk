package egress

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// shopWith is the feature shop client with extra (w17.egress_client) fields.
func shopWith(t *testing.T, fields string) protoreflect.ServiceDescriptor {
	t.Helper()
	src, err := os.ReadFile("wire/testdata/feature_shop.proto")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(src), `server_url: "https://api.feature-shop.example/v2"`, `server_url: "https://api.feature-shop.example/v2"`+"\n  "+fields, 1)
	return compile("clients/feature_shop/feature_shop.proto", s).Services().ByName("FeatureShopApi")
}

func register(t *testing.T, s protoreflect.ServiceDescriptor, dsn string) (*inprocgrpc.Conn, error) {
	t.Helper()
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn := inprocgrpc.New()
	return conn, Register(conn, s, d)
}

// A per-call credential is what the call brought, sent as the DSN's scheme
// says; a call without one is refused before anything is sent.
func TestPerCallCredential(t *testing.T) {
	up := &upstream{t: t, replies: []reply{{status: 200, body: `{"id":"o1","status":"paid","customer":"c"}`}}}
	srv := httptest.NewServer(up)
	defer srv.Close()
	s := shopWith(t, "per_call_credential: true")
	conn, err := register(t, s, srv.URL+"/v2?auth=bearer&token=per_call")
	if err != nil {
		t.Fatal(err)
	}
	in := msgOf(t, s, "GetOrdersByOrderId", `{"orderId":"o1"}`)

	cred := Bearer("tok_customer_1")
	if err := Bind(conn, nil, &cred).Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId")); err != nil {
		t.Fatal(err)
	}
	if got := up.reqs[0].Header.Get("Authorization"); got != "Bearer tok_customer_1" {
		t.Errorf("Authorization = %q", got)
	}

	err = conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId"))
	if status.Code(err) != codes.FailedPrecondition || info(t, err).GetReason() != ReasonNoCredential {
		t.Errorf("no credential: %v", err)
	}
	wrong := Basic("u", "p")
	err = Bind(conn, nil, &wrong).Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId"))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a basic credential for a bearer DSN: %v", err)
	}
	if len(up.reqs) != 1 {
		t.Errorf("%d requests sent; the refused calls must send none", len(up.reqs))
	}
	if s := fmt.Sprintf("%v %+v %#v", cred, cred, cred); strings.Contains(s, "tok_customer_1") {
		t.Errorf("the credential printed: %s", s)
	}
}

// The DSN and the client's declarations must agree, or the boot refuses.
func TestRegister_TheDSNMustMatchThePerCallDeclarations(t *testing.T) {
	for _, c := range []struct{ fields, dsn, want string }{
		{"per_call_credential: true", "https://x.example?auth=bearer&token=real", "write it per_call"},
		{"per_call_credential: true", "https://x.example", "names no auth scheme"},
		{"", "https://x.example?auth=bearer&token=per_call", "does not take one"},
		{"per_call_destination: true", "https://x.example", "must be a policy"},
		{"", "dynamic?timeout=5s", "destination is fixed"},
	} {
		if _, err := register(t, shopWith(t, c.fields), c.dsn); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q + %q: err = %v, want %q", c.fields, c.dsn, err, c.want)
		}
	}
}

// A dynamic client goes where the call says, through the address policy: a
// loopback destination is refused unless the DSN allows it.
func TestPerCallDestination_AndTheAddressPolicy(t *testing.T) {
	up := &upstream{t: t, replies: []reply{{status: 200, body: `{"id":"o1","status":"paid","customer":"c"}`}}}
	srv := httptest.NewServer(up)
	defer srv.Close()
	dest, err := To(srv.URL + "/hooks/")
	if err != nil {
		t.Fatal(err)
	}
	s := shopWith(t, "per_call_destination: true")
	in := msgOf(t, s, "GetOrdersByOrderId", `{"orderId":"o1"}`)

	strict, err := register(t, s, "dynamic?max_attempts=1")
	if err != nil {
		t.Fatal(err)
	}
	err = Bind(strict, &dest, nil).Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId"))
	if status.Code(err) != codes.PermissionDenied || info(t, err).GetReason() != ReasonDestinationRefused {
		t.Fatalf("a loopback destination without allow=loopback: %v", err)
	}
	if len(up.reqs) != 0 {
		t.Fatal("the refused destination was reached")
	}

	open, err := register(t, s, "dynamic?allow=loopback&max_attempts=1")
	if err != nil {
		t.Fatal(err)
	}
	if err := Bind(open, &dest, nil).Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId")); err != nil {
		t.Fatal(err)
	}
	if p := up.reqs[0].URL.Path; p != "/hooks/orders/o1" {
		t.Errorf("path = %q, want the destination's prefix and the call's path", p)
	}
	err = open.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId"))
	if status.Code(err) != codes.FailedPrecondition || info(t, err).GetReason() != ReasonNoDestination {
		t.Errorf("no destination: %v", err)
	}
}

// One destination that keeps failing is suspended; another one of the same
// client is not.
func TestPerCallDestination_AFailingDestinationIsSuspendedAlone(t *testing.T) {
	down := &upstream{t: t, replies: []reply{{status: 503, body: "x"}, {status: 503, body: "x"}, {status: 503, body: "x"}}}
	bad := httptest.NewServer(down)
	defer bad.Close()
	ok := &upstream{t: t, replies: []reply{{status: 200, body: `{"id":"o1","status":"paid","customer":"c"}`}}}
	good := httptest.NewServer(ok)
	defer good.Close()
	s := shopWith(t, "per_call_destination: true")
	conn, err := register(t, s, "dynamic?allow=loopback&max_attempts=1&budget=2&cooldown=1m")
	if err != nil {
		t.Fatal(err)
	}
	in := msgOf(t, s, "GetOrdersByOrderId", `{"orderId":"o1"}`)
	badDest, _ := To(bad.URL)
	goodDest, _ := To(good.URL)
	for i := 0; i < 2; i++ {
		if err := Bind(conn, &badDest, nil).Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId")); status.Code(err) != codes.Unavailable {
			t.Fatalf("call %d to the failing destination: %v", i, err)
		}
	}
	err = Bind(conn, &badDest, nil).Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId"))
	if status.Code(err) != codes.Unavailable || info(t, err).GetReason() != ReasonDestinationSuspended {
		t.Fatalf("third call: %v, want the destination suspended", err)
	}
	if len(down.reqs) != 2 {
		t.Errorf("the suspended destination got %d requests, want 2", len(down.reqs))
	}
	if err := Bind(conn, &goodDest, nil).Invoke(context.Background(), svcPath+"GetOrdersByOrderId", in, outOf(s, "GetOrdersByOrderId")); err != nil {
		t.Errorf("another destination of the same client: %v", err)
	}
}

func TestBudgets_CooldownDoublesAndASuccessClears(t *testing.T) {
	b := newBudgets(1, time.Minute)
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	fail := status.Error(codes.Unavailable, "x")
	b.record("h", fail)
	if b.admit("c", "h") == nil {
		t.Fatal("not suspended after the budget")
	}
	now = now.Add(61 * time.Second)
	if b.admit("c", "h") != nil {
		t.Fatal("still suspended after the cooldown")
	}
	b.record("h", fail)
	now = now.Add(61 * time.Second)
	if b.admit("c", "h") == nil {
		t.Fatal("the second suspension did not double")
	}
	b.record("h", nil)
	if b.admit("c", "h") != nil {
		t.Fatal("a success did not clear the destination")
	}
	b.record("h", status.Error(codes.InvalidArgument, "x"))
	if b.admit("c", "h") != nil {
		t.Fatal("a refusal of the caller's own making counted")
	}
}

func TestTo(t *testing.T) {
	for raw, want := range map[string]string{
		"ftp://x.example":        "scheme",
		"https://":               "no host",
		"https://u:p@x.example":  "credential",
		"https://x.example/?a=1": "query",
		"https://x.example/#f":   "fragment",
	} {
		if _, err := To(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("To(%q) = %v, want %q", raw, err, want)
		}
	}
	if d, err := To(" https://hooks.example.com/w17/ "); err != nil || d.String() != "https://hooks.example.com/w17" {
		t.Errorf("To = %q, %v", d, err)
	}
}

func TestAddressClass(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1": "loopback", "::1": "loopback", "10.1.2.3": "private", "192.168.1.1": "private",
		"172.16.0.1": "private", "100.64.0.1": "private", "169.254.169.254": "link_local", "fe80::1": "link_local",
		"0.0.0.0": "unspecified", "224.0.0.1": "multicast", "8.8.8.8": "", "2a00:1450::1": "", "::ffff:10.0.0.1": "private",
	} {
		if got := addressClass(mustAddr(t, addr)); got != want {
			t.Errorf("%s: %q, want %q", addr, got, want)
		}
	}
}

func TestParseDSN_PerCallAndDynamic(t *testing.T) {
	d, err := ParseDSN("dynamic?auth=api_key&key=per_call&in=header&key_name=X-Hook-Token&allow=private,loopback&budget=3&cooldown=2m")
	if err != nil || !d.Dynamic || !d.PerCall || d.Key != "" || !d.Allow["private"] || d.Budget != 3 || d.Cooldown != 2*time.Minute {
		t.Fatalf("DSN = %+v, %v", d, err)
	}
	if d.String() != "dynamic (auth=api_key)" {
		t.Errorf("String = %q", d.String())
	}
	for dsn, want := range map[string]string{
		"dynamic?allow=public":                                  "allow=",
		"https://x.example?allow=private":                       "unknown parameter",
		"https://x.example?auth=basic&user=u&password=per_call": "fixed user",
		"dynamic?cooldown=1h":                                   "at most",
		"dynamic?budget=0":                                      "positive integer",
	} {
		if _, err := ParseDSN(dsn); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseDSN(%q) = %v, want %q", dsn, err, want)
		}
	}
	if d, err := ParseDSN("https://x.example?auth=basic&user=per_call"); err != nil || !d.PerCall {
		t.Errorf("basic per call: %+v, %v", d, err)
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func msgOf(t *testing.T, s protoreflect.ServiceDescriptor, method, js string) *dynamicpb.Message {
	t.Helper()
	m := dynamicpb.NewMessage(s.Methods().ByName(protoreflect.Name(method)).Input())
	if err := protojson.Unmarshal([]byte(js), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func outOf(s protoreflect.ServiceDescriptor, method string) *dynamicpb.Message {
	return dynamicpb.NewMessage(s.Methods().ByName(protoreflect.Name(method)).Output())
}
