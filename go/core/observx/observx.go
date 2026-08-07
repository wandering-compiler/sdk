// Package observx is the wandering-compiler's unified
// observability lib (REV-031 Phase C-6, 2026-05-09).
// Replaces gox/errorx — same one-liner bootstrap shape but:
//
//   - Carries static service metadata (ServiceName + Version
//   - Environment) so every error / span surfacing in
//     Sentry + OTel is tagged consistently.
//   - Wires both Sentry SDK + OTel TracerProvider from one
//     Config, so generated services don't compose two
//     separate boot calls.
//   - `ReportError(ctx, err)` extracts the OTel active span
//     from ctx, attaches the error (`span.RecordError` +
//     `span.SetStatus(Error)`), and forwards to Sentry with
//     trace_id / span_id / service tags as breadcrumbs — the
//     resulting Sentry event lines up 1:1 with the OTel
//     trace, so the request graph in either tool maps onto
//     the other.
//
// The OTel server span itself is created by the standard
// `otelgrpc.NewServerHandler()` interceptor that generated
// `main.go` wires into the gRPC server. Generated handlers
// (and any business code they call into) read the active
// span from ctx; sub-spans / events / attrs are added via
// the standard OTel API (`otel.Tracer(name).Start(ctx,
// "...")`) — observx doesn't wrap that.
//
// Bootstrap pattern (generated `main.go`):
//
//	func main() {
//	    if err := observx.MustInit(observx.Config{
//	        ServiceName:    "app-storage",
//	        ServiceVersion: os.Getenv("W17_SERVICE_VERSION"),
//	        Environment:    os.Getenv("W17_ENV"),
//	        SentryDSN:      os.Getenv("W17_SENTRY_DSN"),
//	        OTelEndpoint:   os.Getenv("W17_OTEL_ENDPOINT"),
//	    }); err != nil { log.Fatal(err) }
//	    defer observx.Shutdown(context.Background())
//	    ...
//	    s := grpc.NewServer(
//	        grpc.StatsHandler(otelgrpc.NewServerHandler()),
//	        ...
//	    )
//	    ...
//	}
//
// Empty SentryDSN / OTelEndpoint = that exporter stays off;
// `ReportError` falls back to log.Printf so dev / test envs
// don't need any external dep configured.
package observx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// Config carries the bootstrap input for [MustInit]. Only
// ServiceName is required; everything else is opt-in.
//
// REV-033 Cat 5 sweep — env conventions across the project:
//
//   - W17_* envs are project-branded, cross-binary (every
//     storage + gateway binary reads the same key).
//   - <PREFIX>_* envs are per-binary (CORS / TLS / pool
//     tuning vary per deploy).
//   - SDK-canonical envs (OTEL_SERVICE_NAME,
//     OTEL_EXPORTER_OTLP_ENDPOINT) override the
//     corresponding Config fields when set, so k8s OTel-
//     operator-injected envs Just Work without the operator
//     having to set both forms.
//   - Empty env value = unset = default behavior. Explicit
//     "0" / "false" / etc override defaults (e.g.
//     W17_SENTRY_DSN= → stderr fallback; explicit
//     CORS_MAX_AGE=0 → no Max-Age header even though the
//     default is 600s).
//   - Boolean opt-in/opt-out shape follows the feature's
//     posture, not a global polarity rule. Off-by-default
//     features use presence-of-value to enable (set CORS
//     origins → CORS active); on-by-default features use a
//     `_DISABLED=true` knob to turn off (RequestID).
//     Boolean naming within a family stays consistent
//     (CORS uses `_ALLOW_CREDENTIALS` — positive); across
//     families the shape varies for semantic readability
//     ("allow credentials" reads naturally as a positive,
//     while a disable-style knob reads naturally as a
//     negative). New env families pick whichever polarity
//     reads cleanest in isolation, then stay consistent
//     within themselves.
type Config struct {
	// ServiceName identifies the binary in Sentry +
	// OTel as the originating service. Required —
	// MustInit returns an error when empty so misconfig
	// fails loudly at boot rather than silently producing
	// untagged telemetry. Convention: lowercase
	// kebab-case (`app-storage`, `billing-storage`,
	// `app-gateway-rest`).
	ServiceName string

	// ServiceVersion identifies the binary's build —
	// typically a git sha or semver. Empty leaves OTel
	// resource attribute unset; Sentry's Release tag also
	// stays empty (you can configure it via the env after).
	ServiceVersion string

	// Environment identifies the deployment stage
	// ("prod" / "staging" / "dev"). Empty stays empty in
	// both Sentry + OTel resource.
	Environment string

	// SentryDSN enables Sentry reporting when non-empty.
	// Format follows Sentry's standard DSN URL. Empty =
	// no Sentry; ReportError falls through to log.Printf.
	SentryDSN string

	// OTelEndpoint enables OTLP/gRPC trace export when
	// non-empty. Standard OTel collector endpoint shape
	// (`localhost:4317`, `otel-collector:4317`). Empty =
	// no OTel export; ctx-attached spans still work via
	// the global TracerProvider's noop default — handlers
	// can still call `tracer.Start(ctx, ...)` without
	// erroring; spans simply aren't exported anywhere.
	OTelEndpoint string

	// SampleRate is the OTel trace sample rate (0..1).
	// 0 (default) → 1.0 (100% sampling) when OTel is
	// enabled; explicitly set to a fraction for prod
	// where full sampling is too noisy. Sentry's
	// TracesSampleRate stays at gox/sentryx's default 0.2
	// (Sentry has its own pricing-driven sampling story).
	SampleRate float64

	// EnableMetrics turns on a Prometheus MeterProvider
	// alongside the trace TracerProvider. Gateways set this
	// true so otelhttp's auto-instrumentation pipes
	// `http.server.request.duration` etc into a Prom
	// registry that restgw.MountMetricsListener exposes on
	// /metrics. Storage binaries leave it false — they
	// don't run an HTTP listener for /metrics today.
	//
	// Independent of OTelEndpoint — metrics can be on
	// without an OTLP trace exporter (Prom-only deploys),
	// or both, or neither.
	EnableMetrics bool

	// ConfigAttrs is the binary's declared env-var surface,
	// published as OTel resource attributes so a deployed
	// binary's configuration is inspectable from the trace
	// backend. Generated `main.go` builds this from the same
	// lock env declaration the typed EnvConfig comes from.
	//
	// Each non-secret attr with a non-empty value becomes a
	// resource attribute keyed `w17.config.<lower(key)>`. Attrs
	// flagged Secret are dropped ENTIRELY — the value never
	// enters the attribute slice — so a secret cannot leak into
	// telemetry by construction. See [ConfigAttr].
	ConfigAttrs []ConfigAttr
}

// ConfigAttr is one declared env var the binary may publish as an
// OTel resource attribute. The Secret flag is authoritative: a
// secret attr is excluded from export regardless of its Value.
type ConfigAttr struct {
	// Key is the env var name (e.g. "APP_FEATURE_FLAG"). Exported
	// lowercased under the `w17.config.` namespace.
	Key string
	// Value is the resolved env value. Empty values are skipped.
	Value string
	// Secret, when true, drops this attr from the exported set —
	// the value is never added as a resource attribute.
	Secret bool
}

// configResourceAttrs maps the non-secret, non-empty ConfigAttrs to
// OTel resource attributes. Secret attrs are skipped here, before
// they ever reach the attribute slice — the single chokepoint that
// guarantees a secret cannot be exported.
func configResourceAttrs(attrs []ConfigAttr) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		if a.Secret || a.Value == "" {
			continue
		}
		out = append(out, attribute.String("w17.config."+strings.ToLower(a.Key), a.Value))
	}
	return out
}

var (
	mu             sync.Mutex
	tracerProvider *sdktrace.TracerProvider
	otlpExporter   *otlptrace.Exporter
	meterProvider  *sdkmetric.MeterProvider
	sentryEnabled  bool
	debugEvents    bool
	cfg            Config
)

// MustInit panics on bootstrap failure — generated `main.go`
// calls it inside log.Fatal-style boot; the only valid
// recovery from a bad telemetry config is "fix the config
// and restart". Idempotent — second call replaces the
// previous setup (useful for tests; not expected in prod).
func MustInit(c Config) error {
	mu.Lock()
	defer mu.Unlock()

	// Idempotent re-init: tear down any providers/exporters/Sentry a
	// previous MustInit installed before replacing the globals, so a
	// second call (chiefly the test re-init path) doesn't leak the
	// prior batch-span goroutine + OTLP connection. First call is a
	// no-op here (everything is still nil / disabled).
	sdctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = shutdownLocked(sdctx)
	cancel()

	// All-or-nothing: a step BELOW can fail after we've already installed
	// the OTLP exporter + TracerProvider (which holds a gRPC connection and
	// spawns a batch-span goroutine) or wired Sentry — e.g. prometheus.New()
	// failing while EnableMetrics is set. Returning then would orphan those
	// live resources (the same batch-span goroutine + OTLP connection this
	// function takes care not to leak on re-init). Tear down whatever was
	// installed on any error return; the success path flips `success` so this
	// is a no-op. Runs before the deferred mu.Unlock (defers are LIFO), so mu
	// is still held for shutdownLocked.
	success := false
	defer func() {
		if !success {
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = shutdownLocked(cctx)
			ccancel()
		}
	}()

	// REV-033 Cat 5 sweep, F9 — OTEL_SERVICE_NAME (semconv-
	// canonical knob) overrides the codegen-baked Config
	// value. Lets ops re-tag a deployed binary (canary,
	// multi-tenant) without rebuilding. Empty env keeps
	// the Config value.
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		c.ServiceName = v
	}
	if c.ServiceName == "" {
		return errors.New("observx: ServiceName required")
	}

	// REV-033 Cat 5 sweep, F3 — fall back to the SDK-
	// canonical OTEL_EXPORTER_OTLP_ENDPOINT when the
	// branded W17_OTEL_ENDPOINT (which storage/gateway
	// templates pass into Config.OTelEndpoint) is empty.
	// Lets k8s OTel-operator-injected envs Just Work
	// without the operator setting both.
	if c.OTelEndpoint == "" {
		c.OTelEndpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}

	cfg = c

	// W17_OBSERVX_DEBUG turns on the non-exception [ReportEvent]
	// channel (off by default — see ReportEvent). Off-by-default
	// feature, so presence of a truthy value enables it, matching
	// the env posture documented on Config.
	switch strings.ToLower(os.Getenv("W17_OBSERVX_DEBUG")) {
	case "1", "true", "yes", "on":
		debugEvents = true
	default:
		debugEvents = false
	}

	// Sentry init.
	if c.SentryDSN != "" {
		opts := sentry.ClientOptions{
			Dsn:              c.SentryDSN,
			ServerName:       c.ServiceName,
			Release:          c.ServiceVersion,
			Environment:      c.Environment,
			EnableTracing:    true,
			TracesSampleRate: 0.2,
		}
		if err := sentry.Init(opts); err != nil {
			return fmt.Errorf("observx: sentry init: %w", err)
		}
		sentryEnabled = true
	}

	// OTel resource — shared by tracer + meter providers
	// when either is enabled. Only build it when at least
	// one OTel exporter is wired so the noop case stays
	// cheap.
	var res *resource.Resource
	needOTelResource := c.OTelEndpoint != "" || c.EnableMetrics
	if needOTelResource {
		attrs := []attribute.KeyValue{
			semconv.ServiceName(c.ServiceName),
			semconv.ServiceVersion(c.ServiceVersion),
			semconv.DeploymentEnvironmentNameKey.String(c.Environment),
		}
		// Publish the declared non-secret env surface so a
		// deployed binary's config is inspectable; secrets are
		// dropped inside configResourceAttrs by construction.
		attrs = append(attrs, configResourceAttrs(c.ConfigAttrs)...)

		var err error
		res, err = resource.Merge(
			resource.Default(),
			resource.NewWithAttributes(semconv.SchemaURL, attrs...),
		)
		if err != nil {
			return fmt.Errorf("observx: resource merge: %w", err)
		}
	}

	// OTel trace exporter (OTLP/gRPC) → TracerProvider →
	// W3C TextMapPropagator (TraceContext + Baggage) so
	// distributed traces stay linked across services that
	// pass the standard `traceparent` / `tracestate` /
	// `baggage` headers. When OTelEndpoint is empty the
	// global TracerProvider stays at SDK noop default —
	// `tracer.Start(ctx, ...)` calls in generated handlers
	// cost almost nothing.
	if c.OTelEndpoint != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(c.OTelEndpoint),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return fmt.Errorf("observx: otlp exporter: %w", err)
		}
		otlpExporter = exp

		sampleRate := c.SampleRate
		if sampleRate == 0 {
			sampleRate = 1.0
		}
		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.TraceIDRatioBased(sampleRate)),
		)
		otel.SetTracerProvider(tracerProvider)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
	}

	// Prometheus MeterProvider — opt-in (gateway turns it
	// on; storage today doesn't run an HTTP /metrics
	// endpoint). promhttp.Handler() consumed by
	// restgw.MountMetricsListener picks up everything
	// otelhttp / sentry / custom otel.Meter() emit.
	if c.EnableMetrics {
		promExporter, err := prometheus.New()
		if err != nil {
			return fmt.Errorf("observx: prometheus exporter: %w", err)
		}
		meterProvider = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(promExporter),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(meterProvider)
	}
	success = true
	return nil
}

// Shutdown flushes any buffered telemetry and closes the
// exporters. Generated `main.go` defers this from the boot
// path so SIGTERM-graceful shutdown gives Sentry + OTel time
// to push the final batch. Returns the first error
// encountered; callers typically log + ignore (we're
// shutting down anyway).
func Shutdown(ctx context.Context) error {
	mu.Lock()
	defer mu.Unlock()
	return shutdownLocked(ctx)
}

// shutdownLocked is the body of [Shutdown]; the caller MUST hold mu.
// Split out so MustInit can tear down a previous init under the same
// lock without re-acquiring mu (which would deadlock).
func shutdownLocked(ctx context.Context) error {
	var firstErr error
	if tracerProvider != nil {
		if err := tracerProvider.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("observx: tracer shutdown: %w", err)
		}
	}
	if meterProvider != nil {
		if err := meterProvider.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("observx: meter shutdown: %w", err)
		}
	}
	if otlpExporter != nil {
		if err := otlpExporter.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("observx: exporter shutdown: %w", err)
		}
	}
	if sentryEnabled {
		if !sentry.Flush(2*time.Second) && firstErr == nil {
			firstErr = errors.New("observx: sentry flush timed out")
		}
	}
	tracerProvider = nil
	meterProvider = nil
	otlpExporter = nil
	sentryEnabled = false
	return firstErr
}

// ReportError is the unified error-reporting entry point.
// Always:
//
//  1. Attaches the err to the OTel active span (if any) via
//     RecordError + SetStatus(Error) so the trace shows the
//     failed call with the error attached.
//  2. Forwards to Sentry (when configured) with trace_id +
//     span_id + service tags as breadcrumb context. The
//     Sentry event references the OTel trace so devs can
//     pivot between the two tools.
//  3. Falls back to log.Printf when neither Sentry nor OTel
//     is configured — dev / test envs don't need anything
//     external.
//
// nil err is a no-op; safe to defer-call.
// scrubLog escapes CR/LF in a string before it reaches a line-oriented
// log sink, so an error value carrying attacker-influenced text cannot
// forge additional log lines (codeql go/log-injection). Used only on the
// log.Printf fallback paths; the OTel/Sentry sinks structure the value
// themselves and need no scrubbing.
func scrubLog(s string) string {
	return strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(s)
}

func ReportError(ctx context.Context, err error) {
	if err == nil {
		return
	}

	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	mu.Lock()
	enabled := sentryEnabled
	svc := cfg.ServiceName
	mu.Unlock()

	if !enabled {
		// No Sentry → log.Printf fallback. Matches
		// gox/errorx's behavior so the migration is
		// observability-equivalent without Sentry.
		log.Printf("error: %s", scrubLog(err.Error()))
		return
	}

	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		if svc != "" {
			scope.SetTag("service", svc)
		}
		if span.SpanContext().HasTraceID() {
			scope.SetTag("trace_id", span.SpanContext().TraceID().String())
			scope.SetTag("span_id", span.SpanContext().SpanID().String())
		}
		hub.CaptureException(err)
	})
}

// ReportEvent records a NON-exception, debug-grade observability
// event for an expected, already-handled condition that is useful
// when triaging but is NOT a fault — e.g. a DB constraint
// violation that the gRPC layer successfully mapped to a
// user-facing InvalidArgument. Routine, user-correctable failures
// must not raise Sentry issues nor mark the OTel span failed; that
// is exactly what separates them from [ReportError].
//
// Gated behind the W17_OBSERVX_DEBUG knob (off by default): these
// events are pure noise in normal operation and only wanted while
// chasing a specific issue. When off, ReportEvent is a no-op, so
// it stays cheap to leave on hot validation paths. When on it:
//
//  1. Adds an OTel span EVENT (never SetStatus(Error)) so the
//     trace records the condition without marking the span failed.
//  2. Adds a Sentry breadcrumb (never CaptureException) so the
//     context rides along with any LATER real exception on the
//     same hub, without itself raising a Sentry issue.
//  3. Falls back to a `debug:`-prefixed log.Printf when neither
//     exporter is configured.
//
// nil err is a no-op; safe to defer-call.
func ReportEvent(ctx context.Context, err error) {
	if err == nil {
		return
	}

	mu.Lock()
	enabled := debugEvents
	sentryOn := sentryEnabled
	mu.Unlock()

	if !enabled {
		return
	}

	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		// Span EVENT only — deliberately NOT SetStatus(Error),
		// the span stays successful; we just annotate it with the
		// handled condition.
		span.AddEvent("observx.event", trace.WithAttributes(
			attribute.String("event.message", err.Error()),
		))
	}

	if !sentryOn {
		log.Printf("debug: %s", scrubLog(err.Error()))
		return
	}

	sentry.AddBreadcrumb(&sentry.Breadcrumb{
		Category:  "observx",
		Message:   err.Error(),
		Level:     sentry.LevelDebug,
		Timestamp: time.Now(),
	})
}

// Tracer returns the configured tracer for the service.
// Generated handlers + business code call this when adding
// custom sub-spans — `ctx, span := observx.Tracer().Start(ctx,
// "stage-name")`. Returns the global TracerProvider's tracer
// (noop when OTel is disabled).
func Tracer() trace.Tracer {
	// Read cfg under mu — MustInit writes it under the same lock, so
	// reading it unsynchronised would race a concurrent (re-)init.
	mu.Lock()
	name := cfg.ServiceName
	mu.Unlock()
	return otel.Tracer(name)
}
