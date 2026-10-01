package healthcheck_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/wandering-compiler/sdk/go/core/healthz"
	"github.com/wandering-compiler/sdk/go/lib/grpcserver"
	"github.com/wandering-compiler/sdk/go/service/healthcheck"
)

// Every probe here dials a REAL listener on loopback — the thing under test
// is "does a running server answer the probe", which no fake can say.

func noEnv(string) string { return "" }

func run(t *testing.T, args []string, probes ...healthcheck.Probe) (string, error) {
	t.Helper()
	var out bytes.Buffer
	handled, err := healthcheck.Dispatch(context.Background(), append([]string{"health"}, args...), healthcheck.Options{
		Probes: probes,
		Out:    &out,
		Getenv: noEnv,
	})
	if !handled {
		t.Fatal("Dispatch did not handle `health`")
	}
	return out.String(), err
}

// listenHTTP serves h on a wildcard bind, the way a generated listener does
// (":8080"), and returns the address in that same LISTEN form.
func listenHTTP(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return ":" + port
}

func listenGRPC(t *testing.T, status grpc_health_v1.HealthCheckResponse_ServingStatus, opts ...grpc.ServerOption) string {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(opts...)
	hs := health.NewServer()
	hs.SetServingStatus("", status)
	grpc_health_v1.RegisterHealthServer(s, hs)
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return ":" + port
}

// freeAddr is a port nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return ":" + port
}

func TestDispatch_OtherWordsFallThrough(t *testing.T) {
	for _, argv := range [][]string{nil, {}, {"migrate", "apply"}, {"--flag"}, {"healthz"}} {
		handled, err := healthcheck.Dispatch(context.Background(), argv, healthcheck.Options{})
		if handled || err != nil {
			t.Errorf("Dispatch(%q) = (%v, %v), want (false, nil)", argv, handled, err)
		}
	}
}

func TestRun_AllListenersAnswer(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(healthz.Path, healthz.Handler())
	rest := listenHTTP(t, mux)
	grpcAddr := listenGRPC(t, grpc_health_v1.HealthCheckResponse_SERVING)

	out, err := run(t, nil, healthcheck.HTTP("rest", rest), healthcheck.GRPC("grpc", grpcAddr))
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	for _, want := range []string{"ok   rest " + rest, "ok   grpc " + grpcAddr} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A listener that answers but not on /healthz is NOT healthy: it is the
// admin / MCP shape before they mounted the endpoint.
func TestRun_HTTPWithoutHealthzFails(t *testing.T) {
	addr := listenHTTP(t, http.NotFoundHandler())
	out, err := run(t, nil, healthcheck.HTTP("admin", addr))
	if err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("err = %v, want a failure naming admin\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL admin") || !strings.Contains(out, "404") {
		t.Errorf("output should name the listener and the status:\n%s", out)
	}
}

func TestRun_NamesOnlyTheFailingListener(t *testing.T) {
	up := listenGRPC(t, grpc_health_v1.HealthCheckResponse_SERVING)
	down := freeAddr(t)
	out, err := run(t, nil, healthcheck.GRPC("storage", up), healthcheck.HTTP("rest", down))
	if err == nil {
		t.Fatalf("a listener with nothing behind it passed:\n%s", out)
	}
	if got := err.Error(); !strings.Contains(got, "rest") || strings.Contains(got, "storage") {
		t.Errorf("err = %q, want it to name rest and only rest", got)
	}
	if !strings.Contains(out, "ok   storage") || !strings.Contains(out, "FAIL rest") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRun_GRPCNotServingFails(t *testing.T) {
	addr := listenGRPC(t, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	out, err := run(t, nil, healthcheck.GRPC("grpc", addr))
	if err == nil || !strings.Contains(out, "NOT_SERVING") {
		t.Fatalf("err = %v, want NOT_SERVING reported\n%s", err, out)
	}
}

// The timeout bounds the probe: a listener that accepts and never answers
// must fail inside --timeout, not hang the orchestrator's check.
func TestRun_TimeoutBoundsAHungListener(t *testing.T) {
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	addr := listenHTTP(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	start := time.Now()
	_, err := run(t, []string{"--timeout", "200ms"}, healthcheck.HTTP("rest", addr))
	if err == nil {
		t.Fatal("a hung listener passed")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("probe took %s, --timeout was 200ms", took)
	}
}

// A binary with nothing to probe must not report healthy.
func TestRun_NoProbesIsAnError(t *testing.T) {
	if _, err := run(t, nil); err == nil {
		t.Fatal("no probes reported healthy")
	}
}

func TestRun_FlagErrors(t *testing.T) {
	addr := freeAddr(t)
	for _, args := range [][]string{{"--timeout", "0s"}, {"--timeout", "soon"}, {"extra"}, {"--nope"}} {
		if _, err := run(t, args, healthcheck.HTTP("rest", addr)); err == nil {
			t.Errorf("args %q accepted", args)
		}
	}
	out, err := run(t, []string{"--help"}, healthcheck.HTTP("rest", addr))
	if err != nil || !strings.Contains(out, "usage: <binary> health") {
		t.Errorf("--help = (%q, %v)", out, err)
	}
}

// The probe dials what the server LISTENS on: a wildcard bind is loopback; an
// explicit host is dialled as written; a kernel-chosen port cannot be probed.
func TestRun_ListenAddressForms(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(healthz.Path, healthz.Handler())
	wild := listenHTTP(t, mux)
	port := strings.TrimPrefix(wild, ":")

	// "[::]" included: it must dial IPv4 loopback, because a container whose
	// network has no IPv6 has no ::1 even though the dual-stack bind works.
	for _, addr := range []string{wild, "0.0.0.0:" + port, "[::]:" + port, "127.0.0.1:" + port} {
		if out, err := run(t, nil, healthcheck.HTTP("rest", addr)); err != nil {
			t.Errorf("listen %q: %v\n%s", addr, err, out)
		}
	}
	for _, addr := range []string{":0", "nonsense"} {
		if _, err := run(t, nil, healthcheck.HTTP("rest", addr)); err == nil {
			t.Errorf("listen %q: probed as healthy", addr)
		}
	}
}

// Under W17_INTERNAL_TLS the listener serves a leaf issued for its SERVICE
// name, never for loopback. The probe must verify that leaf (no skip-verify)
// and still pass — by naming the server by the container's own leaf SAN.
func TestRun_GRPCUnderInternalTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeLeaf(t, dir, "app-storage.internal")
	env := map[string]string{
		"W17_INTERNAL_TLS":      "on",
		"W17_INTERNAL_TLS_CERT": certPath,
		"W17_INTERNAL_TLS_KEY":  keyPath,
		"W17_INTERNAL_TLS_CA":   certPath,
	}
	getenv := func(k string) string { return env[k] }
	creds, on, err := grpcserver.TLSServerOption(getenv)
	if err != nil || !on {
		t.Fatalf("TLSServerOption = (%v, %v)", on, err)
	}
	addr := listenGRPC(t, grpc_health_v1.HealthCheckResponse_SERVING, creds)

	probe := func(getenv func(string) string) (string, error) {
		var out bytes.Buffer
		err := healthcheck.Run(context.Background(), nil, healthcheck.Options{
			Probes: []healthcheck.Probe{healthcheck.GRPC("grpc", addr)},
			Out:    &out,
			Getenv: getenv,
		})
		return out.String(), err
	}
	if out, err := probe(getenv); err != nil {
		t.Fatalf("TLS probe failed: %v\n%s", err, out)
	}
	// The same listener probed in PLAINTEXT must fail: proves the probe above
	// really took the TLS branch rather than passing by accident.
	if out, err := probe(noEnv); err == nil {
		t.Fatalf("plaintext probe of a TLS listener passed:\n%s", out)
	}
	// A PUBLIC listener in the same container serves plaintext whatever the
	// mesh switch says; PublicGRPC must reach it with the switch ON.
	public := listenGRPC(t, grpc_health_v1.HealthCheckResponse_SERVING)
	var out bytes.Buffer
	if err := healthcheck.Run(context.Background(), nil, healthcheck.Options{
		Probes: []healthcheck.Probe{healthcheck.PublicGRPC("rpc", public)},
		Out:    &out,
		Getenv: getenv,
	}); err != nil {
		t.Fatalf("plaintext public listener failed under the mesh TLS switch: %v\n%s", err, out.String())
	}
	// And with a CA that did not issue the leaf, verification must refuse.
	other, _ := writeLeaf(t, t.TempDir(), "someone-else.internal")
	env["W17_INTERNAL_TLS_CA"] = other
	if out, err := probe(getenv); err == nil {
		t.Fatalf("probe trusted a leaf its CA did not issue:\n%s", out)
	}
}

// writeLeaf writes a self-signed cert whose ONLY SAN is dnsName — no
// localhost, no 127.0.0.1 — so a probe can pass only by naming the server.
func writeLeaf(t *testing.T, dir, dnsName string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, dnsName+".crt")
	keyPath = filepath.Join(dir, dnsName+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
