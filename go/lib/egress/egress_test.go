package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	_ "github.com/wandering-compiler/sdk/go/pb/w17"
	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// shop is the wire package's feature fixture, compiled at run time: a
// generated client's proto with every construct the converter emits.
var shop = func() protoreflect.FileDescriptor {
	src, err := os.ReadFile("wire/testdata/feature_shop.proto")
	if err != nil {
		panic(err)
	}
	return compile("clients/feature_shop/feature_shop.proto", string(src))
}()

func compile(path, src string) protoreflect.FileDescriptor {
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(protocompile.ResolverFunc(func(p string) (protocompile.SearchResult, error) {
		if p == path {
			return protocompile.SearchResult{Source: strings.NewReader(src)}, nil
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

var svc = shop.Services().ByName("FeatureShopApi")

const svcPath = "/clients.feature_shop.FeatureShopApi/"

func init() {
	// No real waiting: record the back-offs instead.
	sleep = func(ctx context.Context, d time.Duration) error {
		sleptMu.Lock()
		slept = append(slept, d)
		sleptMu.Unlock()
		return ctx.Err()
	}
}

var (
	sleptMu sync.Mutex
	slept   []time.Duration
)

func takeSlept() []time.Duration {
	sleptMu.Lock()
	defer sleptMu.Unlock()
	out := slept
	slept = nil
	return out
}

// upstream records every request and answers with the next scripted reply.
type upstream struct {
	t       *testing.T
	mu      sync.Mutex
	reqs    []*http.Request
	bodies  []string
	replies []reply
}

type reply struct {
	status int
	body   string
	header map[string]string
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.reqs = append(u.reqs, r)
	u.bodies = append(u.bodies, string(b))
	rep := reply{status: 500, body: `{"error":"unscripted"}`}
	if len(u.replies) > 0 {
		rep, u.replies = u.replies[0], u.replies[1:]
	}
	u.mu.Unlock()
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

// serve registers the shop client against a fake upstream answering
// replies, behind dsnQuery ("auth=…").
func serve(t *testing.T, dsnQuery string, replies ...reply) (*inprocgrpc.Conn, *upstream) {
	t.Helper()
	takeSlept()
	up := &upstream{t: t, replies: replies}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	dsn, err := ParseDSN(srv.URL + "/v2?" + dsnQuery)
	if err != nil {
		t.Fatal(err)
	}
	conn := inprocgrpc.New()
	if err := Register(conn, svc, dsn); err != nil {
		t.Fatal(err)
	}
	return conn, up
}

func msg(t *testing.T, name, js string) *dynamicpb.Message {
	t.Helper()
	m := dynamicpb.NewMessage(shop.Messages().ByName(protoreflect.Name(name)))
	if js != "" {
		if err := protojson.Unmarshal([]byte(js), m); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// out is an empty response of method's output type.
func out(method string) *dynamicpb.Message {
	return dynamicpb.NewMessage(svc.Methods().ByName(protoreflect.Name(method)).Output())
}

func info(t *testing.T, err error) *errdetails.ErrorInfo {
	t.Helper()
	for _, d := range status.Convert(err).Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			return i
		}
	}
	t.Fatalf("no ErrorInfo on %v", err)
	return nil
}

func TestCall_GetWithQueryHeaderAndAnArrayBody(t *testing.T) {
	conn, up := serve(t, "auth=bearer&token=sk_1", reply{status: 200, body: `[{"id":"o1","status":"paid","customer":"cus_1"},{"id":"o2","status":"in-transit","customer":"cus_2"}]`})
	in := msg(t, "ListOrdersRequest", `{"xRequestId":"rq-1","expand":["customer","lines"],"status":"ORDER_STATUS_PAID","created":{"gte":"5"}}`)
	res := out("ListOrders")
	if err := conn.Invoke(context.Background(), svcPath+"ListOrders", in, res); err != nil {
		t.Fatal(err)
	}
	r := up.reqs[0]
	if r.Method != "GET" || r.URL.Path != "/v2/orders" {
		t.Errorf("request = %s %s, want GET /v2/orders", r.Method, r.URL.Path)
	}
	q := r.URL.Query()
	if got := q["expand"]; len(got) != 2 || got[0] != "customer" || got[1] != "lines" {
		t.Errorf("expand = %v, want the parameter repeated", got)
	}
	if q.Get("status") != "paid" || q.Get("created[gte]") != "5" {
		t.Errorf("query = %v, want status=paid and the deep object created[gte]=5", q)
	}
	if r.Header.Get("X-Request-Id") != "rq-1" || r.Header.Get("Authorization") != "Bearer sk_1" || r.Header.Get("Accept") != "application/json" {
		t.Errorf("headers = %v", r.Header)
	}
	js, _ := protojson.Marshal(res)
	if !strings.Contains(string(js), `"o2"`) || !strings.Contains(string(js), "ORDER_STATUS_IN_TRANSIT") {
		t.Errorf("decoded = %s, want both orders", js)
	}
}

// A POST is retried only because it declares an idempotency header, and
// every attempt carries the one key generated for the call.
func TestCall_IdempotentPostRetriesWithOneKey(t *testing.T) {
	conn, up := serve(t, "auth=basic&user=u&password=p",
		reply{status: 503, body: `busy`},
		reply{status: 429, body: `slow down`, header: map[string]string{"Retry-After": "60"}},
		reply{status: 201, body: `{"id":"o9","status":"requires_payment","customer":"cus_9"}`})
	in := msg(t, "CreateOrderRequest", `{"body":{"customer":"cus_9","metadata":{"ref":"42"}}}`)
	res := out("CreateOrder")
	if err := conn.Invoke(context.Background(), svcPath+"CreateOrder", in, res); err != nil {
		t.Fatal(err)
	}
	if len(up.reqs) != 3 {
		t.Fatalf("attempts = %d, want 3", len(up.reqs))
	}
	key := up.reqs[0].Header.Get("Idempotency-Key")
	for i, r := range up.reqs {
		if k := r.Header.Get("Idempotency-Key"); k == "" || k != key {
			t.Errorf("attempt %d key %q, want the call's one key %q", i+1, k, key)
		}
		if u, p, _ := r.BasicAuth(); u != "u" || p != "p" {
			t.Errorf("attempt %d basic auth %q:%q", i+1, u, p)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type %q, want a form", ct)
		}
	}
	if up.bodies[2] != "customer=cus_9&metadata%5Bref%5D=42" {
		t.Errorf("form body = %q", up.bodies[2])
	}
	// 200 ms after the first failure; Retry-After: 60 capped at 5 s.
	if s := takeSlept(); len(s) != 2 || s[0] != 200*time.Millisecond || s[1] != 5*time.Second {
		t.Errorf("back-offs = %v, want [200ms 5s]", s)
	}
}

// A POST or PATCH without an idempotency header is sent once; a 404 is
// never retried, whatever the method.
func TestCall_NotFoundFailsFast(t *testing.T) {
	if idempotent("POST") || idempotent("PATCH") || !idempotent("GET") || !idempotent("DELETE") {
		t.Fatal("idempotent() table is wrong")
	}
	conn, up := serve(t, "", reply{status: 404, body: `{"message":"no such order"}`})
	err := conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId"))
	if status.Code(err) != codes.NotFound || len(up.reqs) != 1 {
		t.Fatalf("err = %v after %d attempts, want NOT_FOUND after 1 (a 404 is not retried)", err, len(up.reqs))
	}
	i := info(t, err)
	if i.Domain != "feature_shop" || i.Reason != "UPSTREAM_HTTP_404" || i.Metadata["attempts"] != "1" || !strings.Contains(i.Metadata["body"], "no such order") {
		t.Errorf("ErrorInfo = %+v", i)
	}
}

func TestCall_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   codes.Code
		tries  int
	}{
		{400, codes.InvalidArgument, 1},
		{422, codes.InvalidArgument, 1},
		{409, codes.Aborted, 1},
		{401, codes.Internal, 1}, // our credential, not the caller's
		{403, codes.Internal, 1},
		{500, codes.Unavailable, 1}, // the upstream's fault, but not one a retry fixes
		{503, codes.Unavailable, 3},
		{429, codes.Unavailable, 3},
	} {
		replies := make([]reply, 3)
		for i := range replies {
			replies[i] = reply{status: tc.status, body: "x"}
		}
		conn, up := serve(t, "auth=api_key&key=k1&key_name=X-Shop-Key", replies...)
		err := conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId"))
		if status.Code(err) != tc.want || len(up.reqs) != tc.tries {
			t.Errorf("HTTP %d: %v after %d attempts, want %v after %d", tc.status, status.Code(err), len(up.reqs), tc.want, tc.tries)
		}
		if up.reqs[0].Header.Get("X-Shop-Key") != "k1" {
			t.Errorf("api key header missing: %v", up.reqs[0].Header)
		}
		if strings.Contains(err.Error(), "k1") {
			t.Errorf("the credential leaked into the error: %v", err)
		}
	}
}

// A path value is one segment: a `/` in it cannot reach another resource,
// and an empty one is refused rather than addressing the collection.
func TestCall_PathParameters(t *testing.T) {
	conn, up := serve(t, "", reply{status: 204})
	if err := conn.Invoke(context.Background(), svcPath+"DeleteOrder", msg(t, "DeleteOrderRequest", `{"orderId":"a/b?c"}`), out("DeleteOrder")); err != nil {
		t.Fatal(err)
	}
	if got := up.reqs[0].URL.EscapedPath(); got != "/v2/orders/a%2Fb%3Fc" {
		t.Errorf("path = %s, want the value escaped as one segment", got)
	}
	err := conn.Invoke(context.Background(), svcPath+"DeleteOrder", msg(t, "DeleteOrderRequest", `{}`), out("DeleteOrder"))
	if status.Code(err) != codes.InvalidArgument || len(up.reqs) != 1 {
		t.Errorf("empty path parameter: %v (%d requests), want INVALID_ARGUMENT and nothing sent", err, len(up.reqs))
	}
}

func TestCall_APIKeyInQuery(t *testing.T) {
	conn, up := serve(t, "auth=api_key&key=k2&in=query&key_name=apikey", reply{status: 200, body: `{"id":"o1","status":"paid","customer":"c"}`})
	if err := conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId")); err != nil {
		t.Fatal(err)
	}
	if up.reqs[0].URL.Query().Get("apikey") != "k2" {
		t.Errorf("query = %v, want apikey=k2", up.reqs[0].URL.RawQuery)
	}
}

func TestCall_BodyCapAndUndecodable(t *testing.T) {
	conn, _ := serve(t, "", reply{status: 200, body: strings.Repeat("x", MaxResponseBytes+1)})
	err := conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId"))
	if status.Code(err) != codes.Internal || info(t, err).Reason != ReasonTooLarge {
		t.Errorf("oversized body: %v", err)
	}
	conn, _ = serve(t, "", reply{status: 200, body: `{"id": 7}`})
	err = conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId"))
	if status.Code(err) != codes.Internal || info(t, err).Reason != ReasonUndecodable {
		t.Errorf("wrong JSON type: %v", err)
	}
}

func TestCall_TransportErrorIsRetriedThenUnavailable(t *testing.T) {
	dsn, _ := ParseDSN("http://127.0.0.1:1/v2")
	conn := inprocgrpc.New()
	if err := Register(conn, svc, dsn); err != nil {
		t.Fatal(err)
	}
	takeSlept()
	err := conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId"))
	if status.Code(err) != codes.Unavailable || info(t, err).Reason != ReasonTransport || info(t, err).Metadata["attempts"] != "3" {
		t.Errorf("unreachable upstream: %v", err)
	}
	if s := takeSlept(); len(s) != 2 || s[1] != 400*time.Millisecond {
		t.Errorf("back-offs = %v, want [200ms 400ms]", s)
	}
}

// A signature hook runs on every attempt, so a retry is signed afresh.
func TestCall_HooksRunPerAttempt(t *testing.T) {
	var mu sync.Mutex
	n := 0
	BeforeRequest("feature_shop", "GetPet", func(_ context.Context, r *http.Request) error {
		mu.Lock()
		n++
		r.Header.Set("X-Signature", "sig-"+string(rune('0'+n)))
		mu.Unlock()
		return nil
	})
	conn, up := serve(t, "", reply{status: 502}, reply{status: 200, body: `{"petType":"cat"}`})
	if err := conn.Invoke(context.Background(), svcPath+"GetPet", msg(t, "GetPetRequest", `{"id":"1"}`), out("GetPet")); err != nil {
		t.Fatal(err)
	}
	if up.reqs[0].Header.Get("X-Signature") != "sig-1" || up.reqs[1].Header.Get("X-Signature") != "sig-2" {
		t.Errorf("signatures = %q, %q", up.reqs[0].Header.Get("X-Signature"), up.reqs[1].Header.Get("X-Signature"))
	}
	// Another method of the client is not hooked.
	conn, up = serve(t, "", reply{status: 200, body: `{"id":"o1","status":"paid","customer":"c"}`})
	_ = conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId"))
	if up.reqs[0].Header.Get("X-Signature") != "" {
		t.Error("a method hook ran on another method")
	}
}

func TestRegisterFromEnv(t *testing.T) {
	conn := inprocgrpc.New()
	if err := RegisterFromEnv(conn, svc, "W17_CLIENT_FEATURE_SHOP_DSN", func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal(err)
	}
	err := conn.Invoke(context.Background(), svcPath+"GetPet", msg(t, "GetPetRequest", `{"id":"1"}`), out("GetPet"))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "W17_CLIENT_FEATURE_SHOP_DSN") {
		t.Errorf("unconfigured client: %v", err)
	}
	err = RegisterFromEnv(inprocgrpc.New(), svc, "W17_CLIENT_FEATURE_SHOP_DSN", func(string) (string, bool) {
		return "https://api.example?auth=bearer&tokn=secret-value", true
	})
	if err == nil || !strings.Contains(err.Error(), "W17_CLIENT_FEATURE_SHOP_DSN") || strings.Contains(err.Error(), "secret-value") {
		t.Errorf("bad DSN: %v, want an error naming the variable and not the value", err)
	}
}

func TestParseDSN(t *testing.T) {
	d, err := ParseDSN("https://u:p@api.example.com/v1/?auth=basic&timeout=2s&max_attempts=5&proxy=http%3A%2F%2Fproxy%3A3128")
	if err != nil {
		t.Fatal(err)
	}
	if d.User != "u" || d.Password != "p" || d.Timeout != 2*time.Second || d.MaxAttempts != 5 || d.Proxy.Host != "proxy:3128" || d.Base.String() != "https://api.example.com/v1" {
		t.Errorf("dsn = %+v base %s", d, d.Base)
	}
	if s := d.String(); strings.Contains(s, "p@") || strings.Contains(s, ":p") {
		t.Errorf("String() leaks the password: %s", s)
	}
	for _, bad := range []string{
		"ftp://api.example.com",
		"https://?auth=bearer&token=x",
		"https://api.example.com?auth=bearer",
		"https://api.example.com?auth=bearer&token=x&key=y",
		"https://api.example.com?auth=api_key&key=k&in=cookie",
		"https://api.example.com?auth=oauth2_cc&token_url=x",
		"https://api.example.com?auth=magic",
		"https://u:p@api.example.com?auth=bearer&token=x",
		"https://api.example.com?timeout=soon",
		"https://api.example.com?max_attempts=0",
		"https://api.example.com?timeout=30s", // never applies inside the 30 s total
	} {
		if _, err := ParseDSN(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	s := truncate([]byte("ab\xc5\xa1cd"), 3) // cuts inside "š"
	if s != "ab" {
		t.Errorf("truncate = %q, want %q", s, "ab")
	}
	if s := truncate([]byte{0xff, 'a'}, 10); !strings.Contains(s, "a") || !proto.Equal(&errdetails.ErrorInfo{Reason: s}, &errdetails.ErrorInfo{Reason: s}) {
		t.Errorf("invalid bytes: %q", s)
	}
}

// A POST without an idempotency header is sent once: a retry could create
// the order twice.
func TestCall_PostWithoutIdempotencyIsSentOnce(t *testing.T) {
	fd := compile("clients/ledger/ledger.proto", `syntax = "proto3";
package clients.ledger;
import "w17/egress.proto";
option (w17.egress_client) = { name: "ledger" };
service LedgerApi {
  rpc Post(PostRequest) returns (Entry) {
    option (w17.egress_call) = { method: "POST" path: "/entries" request_encoding: EGRESS_ENCODING_JSON };
  }
}
message PostRequest {
  message Body { string memo = 1 [(w17.egress_field) = { name: "memo" }]; }
  Body body = 1 [(w17.egress_field) = { in: EGRESS_IN_BODY required: true }];
}
message Entry { string id = 1 [(w17.egress_field) = { name: "id" }]; }
`)
	ledger := fd.Services().ByName("LedgerApi")
	up := &upstream{t: t, replies: []reply{{status: 503}, {status: 200, body: `{"id":"e1"}`}}}
	srv := httptest.NewServer(up)
	defer srv.Close()
	dsn, _ := ParseDSN(srv.URL)
	conn := inprocgrpc.New()
	if err := Register(conn, ledger, dsn); err != nil {
		t.Fatal(err)
	}
	in := dynamicpb.NewMessage(fd.Messages().ByName("PostRequest"))
	if err := protojson.Unmarshal([]byte(`{"body":{"memo":"m"}}`), in); err != nil {
		t.Fatal(err)
	}
	err := conn.Invoke(context.Background(), "/clients.ledger.LedgerApi/Post", in, dynamicpb.NewMessage(fd.Messages().ByName("Entry")))
	if status.Code(err) != codes.Unavailable || len(up.reqs) != 1 {
		t.Errorf("err = %v after %d requests, want UNAVAILABLE after exactly 1", err, len(up.reqs))
	}
	if up.bodies[0] != `{"memo":"m"}` || up.reqs[0].Header.Get("Content-Type") != "application/json" {
		t.Errorf("body %q (%s)", up.bodies[0], up.reqs[0].Header.Get("Content-Type"))
	}
}

// A transport error's text is the request URL, query and all; an api_key
// sent in the query must not ride out in the status, nor in a retry event.
func TestCall_TransportErrorDoesNotCarryTheQueryKey(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	dsn, err := ParseDSN("http://127.0.0.1:1/v2?auth=api_key&in=query&key_name=apikey&key=SECRET-KEY-1")
	if err != nil {
		t.Fatal(err)
	}
	conn := inprocgrpc.New()
	if err := Register(conn, svc, dsn); err != nil {
		t.Fatal(err)
	}
	err = conn.Invoke(context.Background(), svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want UNAVAILABLE", err)
	}
	if strings.Contains(err.Error(), "SECRET-KEY-1") || strings.Contains(info(t, err).String(), "SECRET-KEY-1") {
		t.Errorf("the key leaked: %v", err)
	}
	retries := 0
	for _, sp := range rec.Ended() {
		if strings.Contains(sp.Status().Description, "SECRET-KEY-1") {
			t.Errorf("span %q status leaks the key: %s", sp.Name(), sp.Status().Description)
		}
		for _, ev := range sp.Events() {
			if ev.Name == "egress.retry" {
				retries++
			}
			for _, a := range ev.Attributes {
				if strings.Contains(a.Value.String(), "SECRET-KEY-1") {
					t.Errorf("span %q event %s leaks the key: %s", sp.Name(), ev.Name, a.Value.String())
				}
			}
		}
	}
	if retries != 2 {
		t.Errorf("retry events = %d, want 2 (the check must have looked at them)", retries)
	}
}

// A deadline that runs out while the call waits to retry is the answer —
// DEADLINE_EXCEEDED, the failure it was waiting on in the metadata — not
// the last attempt's UNAVAILABLE.
func TestCall_DeadlineDuringBackoff(t *testing.T) {
	stub := sleep
	sleep = func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
	t.Cleanup(func() { sleep = stub })
	conn, _ := serve(t, "", reply{status: 503, header: map[string]string{"Retry-After": "5"}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := conn.Invoke(ctx, svcPath+"GetOrdersByOrderId", msg(t, "GetOrdersByOrderIdRequest", `{"orderId":"o1"}`), out("GetOrdersByOrderId"))
	if status.Code(err) != codes.DeadlineExceeded || info(t, err).Metadata["last_failure"] != "HTTP 503" {
		t.Errorf("err = %v (%v), want DEADLINE_EXCEEDED after HTTP 503", err, info(t, err).Metadata)
	}
}

func TestCall_DotSegmentIsRefused(t *testing.T) {
	conn, up := serve(t, "", reply{status: 204})
	for _, v := range []string{".", ".."} {
		err := conn.Invoke(context.Background(), svcPath+"DeleteOrder", msg(t, "DeleteOrderRequest", `{"orderId":"`+v+`"}`), out("DeleteOrder"))
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: %v, want INVALID_ARGUMENT", v, err)
		}
	}
	if len(up.reqs) != 0 {
		t.Errorf("%d requests sent, want none", len(up.reqs))
	}
}
