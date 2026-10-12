package observx

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// collector is an OTLP trace collector on loopback that counts exports.
type collector struct {
	coltracepb.UnimplementedTraceServiceServer
	exports atomic.Int32
}

func (c *collector) Export(context.Context, *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.exports.Add(1)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func startCollector(t *testing.T, opts ...grpc.ServerOption) (*collector, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	s := grpc.NewServer(opts...)
	coltracepb.RegisterTraceServiceServer(s, c)
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
	return c, ln.Addr().String()
}

// writeLoopbackCert writes a self-signed certificate for 127.0.0.1 that
// doubles as its own CA bundle.
func writeLoopbackCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "collector"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "c.crt"), filepath.Join(dir, "c.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// exportOneSpan initialises observx against endpoint, ends one span and
// shuts down (which flushes the batch).
func exportOneSpan(t *testing.T, endpoint string, c Config) error {
	t.Helper()
	resetForTest(t)
	c.ServiceName, c.OTelEndpoint = "svc", endpoint
	if err := MustInit(c); err != nil {
		return err
	}
	_, span := otel.Tracer("svc").Start(context.Background(), "op")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return Shutdown(ctx)
}

// With internal TLS on, telemetry goes to the collector over TLS, verified
// against the mesh CA — the span status descriptions it carries hold error
// text and must not cross the network in plaintext.
func TestOTLPExport_FollowsInternalTLS(t *testing.T) {
	certPath, keyPath := writeLoopbackCert(t)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	col, addr := startCollector(t, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}})))

	t.Setenv("W17_INTERNAL_TLS", "on")
	t.Setenv("W17_INTERNAL_TLS_CA", certPath)
	t.Setenv(EnvOTelTLS, "")
	_ = exportOneSpan(t, addr, Config{})
	if col.exports.Load() == 0 {
		t.Fatal("no export reached the TLS collector with W17_INTERNAL_TLS=on")
	}

	// An explicit TLS setting with the mesh off still dials TLS.
	t.Setenv("W17_INTERNAL_TLS", "")
	before := col.exports.Load()
	_ = exportOneSpan(t, addr, Config{OTelTLS: "on"})
	if col.exports.Load() == before {
		t.Fatal("OTelTLS=on did not export over TLS")
	}
}

// Plaintext is the export transport only when the mesh switch is off, or
// when the operator turns it off explicitly — which is logged when the
// mesh is on.
func TestOTLPExport_PlaintextOnlyWhenOffOrOptedOut(t *testing.T) {
	col, addr := startCollector(t)

	t.Setenv("W17_INTERNAL_TLS", "")
	t.Setenv(EnvOTelTLS, "")
	_ = exportOneSpan(t, addr, Config{})
	if col.exports.Load() == 0 {
		t.Fatal("mesh off: no plaintext export")
	}

	// Mesh on, no opt-out: a plaintext collector is not reached.
	t.Setenv("W17_INTERNAL_TLS", "on")
	before := col.exports.Load()
	_ = exportOneSpan(t, addr, Config{})
	if col.exports.Load() != before {
		t.Fatal("mesh on: telemetry was exported in plaintext")
	}

	// Mesh on, explicit opt-out through the env knob: plaintext, with a warning.
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	t.Setenv(EnvOTelTLS, "off")
	before = col.exports.Load()
	_ = exportOneSpan(t, addr, Config{})
	if col.exports.Load() == before {
		t.Fatal("explicit opt-out did not export in plaintext")
	}
	if !strings.Contains(buf.String(), "plaintext") || !strings.Contains(buf.String(), EnvOTelTLS) {
		t.Errorf("plaintext export under mesh TLS was not logged: %q", buf.String())
	}
}

func TestMustInit_RejectsUnknownOTelTLS(t *testing.T) {
	resetForTest(t)
	err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1", OTelTLS: "required"})
	if err == nil || !strings.Contains(err.Error(), EnvOTelTLS) {
		t.Fatalf("OTelTLS=required: %v, want an error naming %s", err, EnvOTelTLS)
	}
	// The mesh switch's own misconfiguration is refused too, not read as off.
	t.Setenv("W17_INTERNAL_TLS", "enabled")
	if err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1"}); err == nil {
		t.Fatal("W17_INTERNAL_TLS=enabled accepted for the exporter")
	}
}

// A caller's sampling decision is kept: a sampled parent yields a sampled
// child even at a rate that would almost never pick the trace itself.
func TestSampler_HonoursTheParentDecision(t *testing.T) {
	resetForTest(t)
	t.Setenv("W17_INTERNAL_TLS", "")
	if err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1", SampleRate: 1e-9}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Shutdown(context.Background()) })

	// The highest trace id is the one a ratio sampler is least likely to pick.
	var tid trace.TraceID
	for i := range tid {
		tid[i] = 0xff
	}
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	_, span := otel.Tracer("svc").Start(trace.ContextWithRemoteSpanContext(context.Background(), parent), "child")
	defer span.End()
	if !span.SpanContext().IsSampled() {
		t.Error("a sampled parent produced an unsampled child: the caller's decision was ignored")
	}

	unsampled := parent.WithTraceFlags(0)
	_, span2 := otel.Tracer("svc").Start(trace.ContextWithRemoteSpanContext(context.Background(), unsampled), "child")
	defer span2.End()
	if span2.SpanContext().IsSampled() {
		t.Error("an unsampled parent produced a sampled child")
	}
}

func TestMustInit_RejectsSampleRateOutsideZeroToOne(t *testing.T) {
	for _, r := range []float64{-0.5, -1, 1.5, math.NaN()} {
		resetForTest(t)
		if err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1", SampleRate: r}); err == nil {
			_ = Shutdown(context.Background())
			t.Errorf("SampleRate %v accepted", r)
		}
	}
}

// After Shutdown the global providers no longer point at the shut-down
// ones.
func TestShutdown_ResetsGlobalProviders(t *testing.T) {
	resetForTest(t)
	t.Setenv("W17_INTERNAL_TLS", "")
	if err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	tp := tracerProvider
	mu.Unlock()
	if otel.GetTracerProvider() != trace.TracerProvider(tp) {
		t.Fatal("MustInit did not install its tracer provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = Shutdown(ctx)
	if otel.GetTracerProvider() == trace.TracerProvider(tp) {
		t.Error("global tracer provider still points at the shut-down provider")
	}
}

// A failure after the trace provider was installed leaves no global
// pointing at it.
func TestMustInit_FailureResetsGlobalProviders(t *testing.T) {
	resetForTest(t)
	t.Setenv("W17_INTERNAL_TLS", "")
	prev := otel.GetTracerProvider()
	newPrometheusExporter = func(...prometheus.Option) (*prometheus.Exporter, error) {
		return nil, errors.New("registry refused")
	}
	t.Cleanup(func() { newPrometheusExporter = prometheus.New })

	if err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1", EnableMetrics: true}); err == nil {
		t.Fatal("want the exporter failure")
	}
	if got := otel.GetTracerProvider(); got != prev {
		if _, isSDK := got.(*sdktrace.TracerProvider); isSDK {
			t.Error("global tracer provider is the shut-down provider of the failed init")
		}
	}
}
