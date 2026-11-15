package egress

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

var soapFile = func() protoreflect.FileDescriptor {
	src, err := os.ReadFile("wire/testdata/feature_soap.proto")
	if err != nil {
		panic(err)
	}
	return compile("clients/feature_soap/feature_soap.proto", string(src))
}()

const soapPath = "/clients.feature_soap.FeatureSoapApi/"

func serveSOAP(t *testing.T, replies ...reply) (*inprocgrpc.Conn, *upstream) {
	t.Helper()
	takeSlept()
	up := &upstream{t: t, replies: replies}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	dsn, err := ParseDSN(srv.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	conn := inprocgrpc.New()
	if err := Register(conn, soapFile.Services().ByName("FeatureSoapApi"), dsn); err != nil {
		t.Fatal(err)
	}
	return conn, up
}

func soapMsg(t *testing.T, name, js string) *dynamicpb.Message {
	t.Helper()
	m := dynamicpb.NewMessage(soapFile.Messages().ByName(protoreflect.Name(name)))
	if err := protojson.Unmarshal([]byte(js), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func soapOut(name string) *dynamicpb.Message {
	return dynamicpb.NewMessage(soapFile.Messages().ByName(protoreflect.Name(name)))
}

const envelope = `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>%s</s:Body></s:Envelope>`

func env(body string) string { return strings.Replace(envelope, "%s", body, 1) }

func TestSOAP_CallSendsTheEnvelopeAndDecodes(t *testing.T) {
	conn, up := serveSOAP(t,
		reply{status: 200, body: env(`<r:StatusResponse xmlns:r="urn:feature"><r:status statusCode="0"/><r:payer dic="CZ1" nespolehlivyPlatce="NE"/></r:StatusResponse>`)})
	res := soapOut("StatusResponse")
	if err := conn.Invoke(context.Background(), soapPath+"GetStatus", soapMsg(t, "StatusRequest", `{"dic":["CZ1"]}`), res); err != nil {
		t.Fatal(err)
	}
	r := up.reqs[0]
	if r.Method != "POST" || r.URL.Path != "/ws" {
		t.Errorf("request %s %s, want POST /ws (every operation posts to the endpoint)", r.Method, r.URL.Path)
	}
	if got := r.Header.Get("SOAPAction"); got != `"urn:feature/getStatus"` {
		t.Errorf("SOAPAction %q", got)
	}
	if got := r.Header.Get("Content-Type"); got != "text/xml; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
	if !strings.Contains(up.bodies[0], `<ns1:StatusRequest><ns1:dic>CZ1</ns1:dic></ns1:StatusRequest>`) {
		t.Errorf("body %s", up.bodies[0])
	}
	got, _ := protojson.Marshal(res)
	if !strings.Contains(string(got), `"statusCode":"0"`) || !strings.Contains(string(got), `"RELIABILITY_NE"`) {
		t.Errorf("decoded %s", got)
	}
}

// A SOAP call is always a POST: only an operation the client marks
// idempotent is retried; a Client fault never is, whatever the operation.
func TestSOAP_RetryAndFaults(t *testing.T) {
	serverFault := env(`<s:Fault><faultcode>s:Server</faultcode><faultstring>busy</faultstring></s:Fault>`)
	clientFault := env(`<s:Fault><faultcode>s:Client</faultcode><faultstring>bad DIC</faultstring><detail>CZ? is no DIC</detail></s:Fault>`)
	ok := env(`<StatusResponse xmlns="urn:feature"><status statusCode="0"/></StatusResponse>`)

	conn, up := serveSOAP(t, reply{status: 503, body: serverFault}, reply{status: 200, body: ok})
	if err := conn.Invoke(context.Background(), soapPath+"GetStatus", soapMsg(t, "StatusRequest", `{"dic":["CZ1"]}`), soapOut("StatusResponse")); err != nil || len(up.reqs) != 2 {
		t.Errorf("idempotent operation after a 503 fault: %v after %d attempts, want success after 2", err, len(up.reqs))
	}

	conn, up = serveSOAP(t, reply{status: 503, body: serverFault}, reply{status: 200, body: ok})
	err := conn.Invoke(context.Background(), soapPath+"Register", soapMsg(t, "RegisterRequest", `{"name":"x"}`), soapOut("RegisterResponse"))
	if status.Code(err) != codes.Unavailable || len(up.reqs) != 1 {
		t.Errorf("non-idempotent operation: %v after %d attempts, want UNAVAILABLE after 1", err, len(up.reqs))
	}
	if i := info(t, err); i.Reason != ReasonSOAPFault || i.Metadata["fault_code"] != "Server" || !strings.Contains(err.Error(), "busy") {
		t.Errorf("server fault ErrorInfo %+v (%v)", i, err)
	}

	conn, up = serveSOAP(t, reply{status: 500, body: clientFault}, reply{status: 500, body: clientFault})
	err = conn.Invoke(context.Background(), soapPath+"GetStatus", soapMsg(t, "StatusRequest", `{"dic":["CZ?"]}`), soapOut("StatusResponse"))
	if status.Code(err) != codes.InvalidArgument || len(up.reqs) != 1 {
		t.Errorf("client fault: %v after %d attempts, want INVALID_ARGUMENT after 1", err, len(up.reqs))
	}
	if i := info(t, err); i.Metadata["detail"] != "CZ? is no DIC" {
		t.Errorf("client fault ErrorInfo %+v", i)
	}

	// A fault answered with 200 is still a fault.
	conn, _ = serveSOAP(t, reply{status: 200, body: clientFault})
	err = conn.Invoke(context.Background(), soapPath+"GetStatus", soapMsg(t, "StatusRequest", `{"dic":["CZ?"]}`), soapOut("StatusResponse"))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("fault with 200: %v, want INVALID_ARGUMENT", err)
	}

	// A 5xx without an envelope is an HTTP failure, as for REST.
	conn, _ = serveSOAP(t, reply{status: 502, body: "bad gateway"}, reply{status: 502, body: "bad gateway"}, reply{status: 502, body: "bad gateway"})
	err = conn.Invoke(context.Background(), soapPath+"GetStatus", soapMsg(t, "StatusRequest", `{"dic":["CZ1"]}`), soapOut("StatusResponse"))
	if i := info(t, err); status.Code(err) != codes.Unavailable || i.Reason != "UPSTREAM_HTTP_502" {
		t.Errorf("plain 502: %v %+v", err, i)
	}
}
