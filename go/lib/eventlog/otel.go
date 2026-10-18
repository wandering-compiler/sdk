package eventlog

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/core/observx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// emitTelemetry writes one sampled record to the telemetry backend as an OTel
// log record, carrying the request's trace and span ids so the backend joins
// it to the span (a span cannot be added to once it ended). The body is the
// record as JSON — meta, the redacted request and response — decoded from
// the bytes the envelope already holds, so it is exactly what the stream
// carries. Runs in the batcher, off the request path.
func emitTelemetry(rec *Record, meta *w17pb.RequestEventMeta, req, resp []byte) {
	body := map[string]json.RawMessage{}
	if b, err := protojson.Marshal(meta); err == nil {
		body["meta"] = b
	}
	if rec.req != nil && len(req) > 0 {
		if b := redactedJSON(rec.req, req); b != nil {
			body["request"] = b
		}
	}
	if rec.resp != nil && len(resp) > 0 {
		if b := redactedJSON(rec.resp, resp); b != nil {
			body["response"] = b
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	var lr otellog.Record
	lr.SetTimestamp(rec.start)
	lr.SetObservedTimestamp(time.Now())
	lr.SetSeverity(otellog.SeverityInfo)
	lr.SetSeverityText("INFO")
	lr.SetEventName("w17.request")
	lr.SetBody(attribute.StringValue(string(raw)))
	lr.AddAttributes(
		attribute.String("w17.request.domain", rec.s.r.domain),
		attribute.String("w17.request.api", apiToken(rec.s.api)),
		attribute.String("w17.request.method", rec.method.FullName),
		attribute.Int("w17.request.grpc_code", int(rec.grpcCode)),
		attribute.Int64("w17.request.duration_us", rec.dur.Microseconds()),
	)
	if rec.httpStatus != 0 {
		lr.AddAttributes(attribute.Int("http.response.status_code", int(rec.httpStatus)))
	}
	ctx := context.Background()
	if rec.span.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, rec.span)
	}
	observx.RequestLogger().Emit(ctx, lr)
}

// redactedJSON renders the redacted bytes of a message of m's type.
func redactedJSON(m proto.Message, b []byte) json.RawMessage {
	view := m.ProtoReflect().New().Interface()
	if proto.Unmarshal(b, view) != nil {
		return nil
	}
	out, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(view)
	if err != nil {
		return nil
	}
	return out
}
