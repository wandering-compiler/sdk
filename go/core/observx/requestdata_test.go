package observx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	lognoop "go.opentelemetry.io/otel/log/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/redact"
)

// The request of these tests: a generated message whose field 7
// (default_value) the plan marks sensitive and carries the decoy.
func init() {
	redact.Register(map[string]redact.Plan{
		"google.protobuf.FieldDescriptorProto": {Skip: []protowire.Number{7}},
	})
}

func sampleRequest() *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:         proto.String("Jana Nováková"),
		Number:       proto.Int32(7),
		DefaultValue: proto.String("DECOY-secret"),
	}
}

func callerCtx() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-w17-scope-org_id", "org-42", "x-w17-scope-user_id", "u-7"))
}

func TestReportError_CarriesTheRedactedRequest(t *testing.T) {
	events := captureSentry(t)
	silenceLog(t)
	ctx := WithRequest(callerCtx(), "/acme.v1.Users/SignUp", sampleRequest())
	ReportError(ctx, errors.New("db down"))

	got := events()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	e := got[0]
	for k, want := range map[string]string{"method": "/acme.v1.Users/SignUp", "org_id": "org-42", "user_id": "u-7"} {
		if e.Tags[k] != want {
			t.Errorf("tag %s = %q, want %q", k, e.Tags[k], want)
		}
	}
	rc := e.Contexts["request"]
	raw, _ := json.Marshal(rc)
	if strings.Contains(string(raw), "DECOY") {
		t.Fatalf("a sensitive value reached Sentry: %s", raw)
	}
	data, _ := rc["data"].(map[string]any)
	if data["name"] != "Jana Nováková" || data["number"] != float64(7) {
		t.Fatalf("request context lacks the plain fields: %s", raw)
	}
	if rc["type"] != "google.protobuf.FieldDescriptorProto" || rc["method"] != "/acme.v1.Users/SignUp" {
		t.Fatalf("request context: %s", raw)
	}
}

func TestReportError_ModeErrorsStillCarriesIt_ModeOffDoesNot(t *testing.T) {
	for _, c := range []struct {
		mode string
		want bool
	}{{"errors", true}, {"off", false}, {"", true}} {
		t.Run(c.mode, func(t *testing.T) {
			t.Setenv(EnvRequestData, c.mode)
			events := captureSentry(t)
			silenceLog(t)
			ReportError(WithRequest(context.Background(), "/m", sampleRequest()), errors.New("x"))
			_, has := events()[0].Contexts["request"]
			if has != c.want {
				t.Fatalf("mode %q: request context present = %v", c.mode, has)
			}
		})
	}
}

func TestReportError_WithoutSentryLogsTheRequest(t *testing.T) {
	resetForTest(t)
	buf := silenceLog(t)
	ReportError(WithRequest(context.Background(), "/acme.v1.Users/SignUp", sampleRequest()), errors.New("db down"))
	line := buf.String()
	if !strings.Contains(line, `method=/acme.v1.Users/SignUp`) || !strings.Contains(line, `"name":"Jana Nováková"`) {
		t.Fatalf("log line lacks the request: %q", line)
	}
	if strings.Contains(line, "DECOY") {
		t.Fatalf("a sensitive value reached the log: %q", line)
	}
}

func TestReportError_SpanEventOnlyOnTheFailingSpan(t *testing.T) {
	resetForTest(t)
	silenceLog(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("t").Start(context.Background(), "failing")
	ReportError(WithRequest(ctx, "/m", sampleRequest()), errors.New("boom"))
	span.End()
	_, ok := tp.Tracer("t").Start(WithRequest(context.Background(), "/m", sampleRequest()), "fine")
	ok.End()

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	var data string
	for _, ev := range spans[0].Events() {
		if ev.Name == "observx.request" {
			for _, a := range ev.Attributes {
				if a.Key == attribute.Key("request.data") {
					data = a.Value.AsString()
				}
			}
		}
	}
	if !strings.Contains(data, `"name":"Jana Nováková"`) || strings.Contains(data, "DECOY") {
		t.Fatalf("span event request.data = %q", data)
	}
	if len(spans[1].Events()) != 0 {
		t.Fatalf("a span that did not fail got events: %v", spans[1].Events())
	}
}

func TestReportError_TypeWithoutPlanIsOmitted(t *testing.T) {
	events := captureSentry(t)
	silenceLog(t)
	ReportError(WithRequest(context.Background(), "/m", wrapperspb.String("DECOY")), errors.New("x"))
	rc := events()[0].Contexts["request"]
	raw, _ := json.Marshal(rc)
	if strings.Contains(string(raw), "DECOY") || rc["omitted"] == nil {
		t.Fatalf("a type without a plan: %s", raw)
	}
}

func TestReportError_LargeRequestIsCut(t *testing.T) {
	events := captureSentry(t)
	silenceLog(t)
	req := sampleRequest()
	req.Name = proto.String(strings.Repeat("n", 100_000))
	ReportError(WithRequest(context.Background(), "/m", req), errors.New("x"))
	rc := events()[0].Contexts["request"]
	if cut, _ := rc["truncated"].([]string); len(cut) != 1 || cut[0] != "name" {
		t.Fatalf("truncated = %v", rc["truncated"])
	}
	if n := len(rc["data"].(map[string]any)["name"].(string)); n > requestDataCeiling {
		t.Fatalf("name kept %d bytes", n)
	}
}

func TestRequestDataEnv(t *testing.T) {
	for _, c := range []struct {
		mode, sample string
		ok           bool
	}{
		{"all", "0.25", true}, {"ERRORS", "", true}, {"off", "1", true},
		{"some", "", false}, {"all", "1.5", false}, {"all", "-0.1", false}, {"all", "NaN", false}, {"all", "x", false},
	} {
		t.Run(c.mode+"/"+c.sample, func(t *testing.T) {
			resetForTest(t)
			t.Setenv(EnvRequestData, c.mode)
			t.Setenv(EnvRequestDataSample, c.sample)
			err := MustInit(Config{ServiceName: "svc"})
			if (err == nil) != c.ok {
				t.Fatalf("MustInit err = %v, want ok=%v", err, c.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "W17_OBSERVX_REQUEST_DATA") {
				t.Fatalf("error does not name the knob: %v", err)
			}
		})
	}
}

func TestSampleRequestData(t *testing.T) {
	for _, c := range []struct {
		mode, sample string
		want         bool
	}{{"all", "1", true}, {"all", "0", false}, {"errors", "1", false}, {"off", "1", false}} {
		resetForTest(t)
		t.Setenv(EnvRequestData, c.mode)
		t.Setenv(EnvRequestDataSample, c.sample)
		if err := MustInit(Config{ServiceName: "svc"}); err != nil {
			t.Fatal(err)
		}
		if got := SampleRequestData(); got != c.want {
			t.Errorf("%s/%s: SampleRequestData = %v", c.mode, c.sample, got)
		}
	}
	resetForTest(t)
	t.Setenv(EnvRequestData, "all")
	t.Setenv(EnvRequestDataSample, "0.5")
	if err := MustInit(Config{ServiceName: "svc"}); err != nil {
		t.Fatal(err)
	}
	n := 0
	for i := 0; i < 4000; i++ {
		if SampleRequestData() {
			n++
		}
	}
	if n < 1600 || n > 2400 {
		t.Errorf("sample 0.5: %d of 4000", n)
	}
}

func TestWithRequest_OffStoresNothing(t *testing.T) {
	resetForTest(t)
	t.Setenv(EnvRequestData, "off")
	if err := MustInit(Config{ServiceName: "svc"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if WithRequest(ctx, "/m", sampleRequest()) != ctx {
		t.Fatal("mode off stored the request")
	}
}

func TestSampleRequestTelemetry_NeedsASink(t *testing.T) {
	resetForTest(t)
	if err := MustInit(Config{ServiceName: "svc"}); err != nil {
		t.Fatal(err)
	}
	if SampleRequestTelemetry() {
		t.Fatal("telemetry sampled with no sink installed")
	}
	UseRequestLogger(lognoop.NewLoggerProvider())
	t.Cleanup(func() { UseRequestLogger(nil) })
	if !SampleRequestTelemetry() {
		t.Fatal("mode all, sample 1, a sink: not sampled")
	}
	resetForTest(t)
	t.Setenv(EnvRequestData, "errors")
	if err := MustInit(Config{ServiceName: "svc"}); err != nil {
		t.Fatal(err)
	}
	UseRequestLogger(lognoop.NewLoggerProvider())
	if SampleRequestTelemetry() {
		t.Fatal("mode errors sends every request to telemetry")
	}
}

// With an OTel endpoint and mode all, MustInit installs the logs pipeline
// as the sink; mode errors does not; Shutdown removes it.
func TestMustInit_InstallsTheRequestLogsPipeline(t *testing.T) {
	for _, c := range []struct {
		mode string
		want bool
	}{{"all", true}, {"errors", false}} {
		resetForTest(t)
		t.Setenv(EnvRequestData, c.mode)
		t.Setenv(EnvOTelTLS, "off")
		if err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1"}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := requestLogger != nil
		mu.Unlock()
		if got != c.want {
			t.Errorf("mode %s: sink installed = %v", c.mode, got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = Shutdown(ctx)
		cancel()
		mu.Lock()
		left := requestLogger != nil
		mu.Unlock()
		if left {
			t.Errorf("mode %s: Shutdown left the sink", c.mode)
		}
	}
}
