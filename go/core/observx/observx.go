// Package observx is the wandering-compiler's unified
// observability lib.
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
//
// Breadcrumbs belong to one request: an entry point (the gRPC
// recovery interceptor, the REST recovery middleware, an MCP tool
// call) gives the request its own Sentry hub with [WithRequestScope],
// and [ReportEvent] / [ReportRefusal] add breadcrumbs only to the hub
// carried by their context. Nothing is added to the process-wide hub,
// so one request's breadcrumbs are never attached to another
// request's event.
//
// The log lines the package writes (the stderr fallback of
// [ReportError] and [ReportEvent], and every [ReportRefusal]) are
// rate-limited per channel; lines over the budget are counted and the
// count is logged with the next line that is written (and on
// [Shutdown]), so a flood of client-driven refusals cannot become an
// equal flood of log writes.
package observx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
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
	// The semconv version MUST be the one go.opentelemetry.io/otel/sdk's
	// resource.Default() uses: merging resources with different schema
	// URLs is an error, and MustInit returns it — every generated binary
	// with OTel on would refuse to boot. TestMustInit_FullStack catches a
	// mismatch; bump this with the SDK (v1.45.0 uses v1.43.0).
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/credentials"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// Config carries the bootstrap input for [MustInit]. Only
// ServiceName is required; everything else is opt-in.
//
// Env conventions across the project:
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

	// OTelTLS is the transport security of the OTLP export.
	// Empty falls back to the W17_OTEL_TLS env var; both empty
	// means "follow the internal mesh": the exporter dials TLS
	// exactly when a w17 client would (W17_INTERNAL_TLS on or
	// terminated), with the same CA and client pair, and
	// plaintext only when the mesh switch is off.
	//
	//   - "on" / "true" / "1" / "yes": TLS whatever the mesh
	//     switch says (W17_INTERNAL_TLS_CA and the client pair
	//     are used when set, else the system roots).
	//   - "off" / "false" / "0" / "no": plaintext, the explicit
	//     operator opt-out. With the mesh switch on this is
	//     logged as a warning at init.
	//
	// Any other value fails [MustInit]. Span status descriptions
	// carry error text, so telemetry travels with the same
	// protection as the traffic it describes.
	OTelTLS string

	// SampleRate is the OTel trace sample rate for traces
	// this service starts (0..1). 0 (default) → 1.0 (100%
	// sampling) when OTel is enabled; explicitly set to a
	// fraction for prod where full sampling is too noisy. A
	// request that arrives with a sampling decision from its
	// caller keeps that decision (parent-based), so a trace
	// is not cut into pieces by services with different
	// rates. A value outside 0..1 (or NaN) fails [MustInit].
	// Sentry's TracesSampleRate stays at gox/sentryx's
	// default 0.2 (Sentry has its own pricing-driven
	// sampling story).
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
	sentryClient   *sentry.Client // the client MustInit bound; nil when off
	debugEvents    bool
	cfg            Config
)

// EnvOTelTLS is the env fallback for [Config.OTelTLS].
const EnvOTelTLS = "W17_OTEL_TLS"

// Test seams.
var (
	newPrometheusExporter = prometheus.New
	// sentryBeforeSend, when set, is installed as Sentry's BeforeSend.
	sentryBeforeSend func(*sentry.Event, *sentry.EventHint) *sentry.Event
)

// MustInit initialises observability and returns an error on
// bootstrap failure (it does not panic, despite the name) —
// generated `main.go` calls it inside log.Fatal-style boot; the
// only valid recovery from a bad telemetry config is "fix the
// config and restart". On failure nothing it installed is left
// behind: exporters are shut down and the global OTel providers
// are reset. A second call replaces the previous setup (useful for
// tests; not expected in prod) — unless a supervisor holds the state
// through [Acquire]: then it is refused with [ErrHeld], because
// replacing the providers would swap telemetry under components that
// are running.
func MustInit(c Config) error {
	mu.Lock()
	defer mu.Unlock()
	if holders > 0 {
		return heldError("MustInit")
	}
	return initLocked(c)
}

// initLocked is the body of [MustInit]; the caller MUST hold mu.
func initLocked(c Config) error {
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

	// OTEL_SERVICE_NAME (semconv-
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

	// Fall back to the SDK-
	// canonical OTEL_EXPORTER_OTLP_ENDPOINT when the
	// branded W17_OTEL_ENDPOINT (which storage/gateway
	// templates pass into Config.OTelEndpoint) is empty.
	// Lets k8s OTel-operator-injected envs Just Work
	// without the operator setting both.
	if c.OTelEndpoint == "" {
		c.OTelEndpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if c.OTelTLS == "" {
		c.OTelTLS = os.Getenv(EnvOTelTLS)
	}
	if math.IsNaN(c.SampleRate) || c.SampleRate < 0 || c.SampleRate > 1 {
		return fmt.Errorf("observx: SampleRate %v is outside 0..1 (0 means sample every trace)", c.SampleRate)
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
			BeforeSend:       sentryBeforeSend,
		}
		if err := sentry.Init(opts); err != nil {
			return fmt.Errorf("observx: sentry init: %w", err)
		}
		sentryEnabled = true
		sentryClient = sentry.CurrentHub().Client()
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
		transport, err := otlpTransport(c.OTelTLS, os.Getenv)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(c.OTelEndpoint),
			transport,
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
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRate))),
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
		promExporter, err := newPrometheusExporter()
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

// otlpTransport picks the OTLP exporter's transport security from the
// OTelTLS setting and the internal-mesh switch read through lookup (see
// [Config.OTelTLS]).
func otlpTransport(setting string, lookup func(string) string) (otlptracegrpc.Option, error) {
	var tlsLookup grpcclient.LookupFunc
	switch strings.ToLower(strings.TrimSpace(setting)) {
	case "":
		// Follow the mesh: TLS exactly when a w17 client dials TLS.
		tlsLookup = lookup
	case "on", "true", "1", "yes":
		mesh := lookup(grpcclient.EnvInternalTLS)
		tlsLookup = func(k string) string {
			if k == grpcclient.EnvInternalTLS && !grpcclient.InternalTLSDial(mesh) {
				return "on"
			}
			return lookup(k)
		}
	case "off", "false", "0", "no":
		if mesh := lookup(grpcclient.EnvInternalTLS); grpcclient.InternalTLSDial(mesh) {
			log.Printf("observx: telemetry is exported in plaintext although %s=%s, because %s is off",
				grpcclient.EnvInternalTLS, mesh, EnvOTelTLS)
		}
		return otlptracegrpc.WithInsecure(), nil
	default:
		return nil, fmt.Errorf("observx: %s=%q is not a recognised value; accepted: on|true|1|yes (TLS), off|false|0|no (plaintext), or unset (follow %s)",
			EnvOTelTLS, setting, grpcclient.EnvInternalTLS)
	}
	tlsCfg, err := grpcclient.ClientTLSConfig(tlsLookup)
	if err != nil {
		return nil, fmt.Errorf("observx: otlp exporter TLS: %w", err)
	}
	if tlsCfg == nil {
		return otlptracegrpc.WithInsecure(), nil
	}
	return otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsCfg)), nil
}

// Shutdown flushes any buffered telemetry and closes the
// exporters. Generated `main.go` defers this from the boot
// path so SIGTERM-graceful shutdown gives Sentry + OTel time
// to push the final batch. Returns the first error
// encountered; callers typically log + ignore (we're
// shutting down anyway).
//
// While a supervisor holds the state through [Acquire], Shutdown
// is refused with [ErrHeld] and tears nothing down; the last
// holder's release shuts down instead.
func Shutdown(ctx context.Context) error {
	mu.Lock()
	defer mu.Unlock()
	if holders > 0 {
		return heldError("Shutdown")
	}
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
	// The globals must not keep pointing at a provider that is shut down:
	// spans and instruments taken from them afterwards would be dropped
	// silently. Reset only what this package installed, so a provider
	// someone else set in the meantime is left alone.
	if tracerProvider != nil && otel.GetTracerProvider() == trace.TracerProvider(tracerProvider) {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
	}
	if meterProvider != nil && otel.GetMeterProvider() == metric.MeterProvider(meterProvider) {
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
	}
	// Same for Sentry: a client left bound on the global hub kept sending
	// for anyone calling sentry directly after Shutdown — and after a
	// MustInit that failed part-way, which is meant to install nothing.
	if sentryClient != nil {
		if sentry.CurrentHub().Client() == sentryClient {
			sentry.CurrentHub().BindClient(nil)
		}
		sentryClient.Close()
		sentryClient = nil
	}
	for _, l := range limiters {
		l.flush()
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
		errorLog.printf("error: %s", scrubLog(err.Error()))
		return
	}

	// The request's own hub carries its breadcrumbs. Without one, a clone
	// of the process hub is used with its breadcrumbs dropped: whatever
	// sits there was not recorded for this request.
	hub := sentry.GetHubFromContext(ctx)
	ownHub := hub != nil
	if !ownHub {
		hub = sentry.CurrentHub()
	}
	hub = hub.Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		if !ownHub {
			scope.ClearBreadcrumbs()
		}
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
		eventLog.printf("debug: %s", scrubLog(err.Error()))
		return
	}

	addBreadcrumb(ctx, &sentry.Breadcrumb{
		Category:  "observx",
		Message:   err.Error(),
		Level:     sentry.LevelDebug,
		Timestamp: time.Now(),
	})
}

// ReportRefusal records a request the surface REFUSED — a caller without the
// permission, an input that would not parse, a scope that did not match.
//
// It sits between the other two on purpose. [ReportError] is for faults
// somebody has to look at: it marks the span failed and raises a Sentry issue,
// which a caller sending a malformed id should never do. [ReportEvent] is for
// context worth carrying if something later goes wrong, and it is SILENT unless
// W17_OBSERVX_DEBUG is on.
//
// A refusal fits neither. It is the expected outcome of a request, so it is not
// an exception — and it is the only record of WHY the caller was turned away,
// so it must not be silent. The reason a gateway used to put "missing
// permission tasks.TaskQuery.ListTasksPaged" in the response body is that the
// body was the only place it went; routing it to a channel that is off by
// default would move that leak rather than close it, and would leave an
// operator with a 403 and no reason on either side.
//
// So: span EVENT (never SetStatus(Error)), Sentry breadcrumb (never
// CaptureException), and a log line — always, with no knob in front of it.
//
// nil err is a no-op; safe to defer-call.
func ReportRefusal(ctx context.Context, err error) {
	if err == nil {
		return
	}

	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.AddEvent("observx.refusal", trace.WithAttributes(
			attribute.String("event.message", err.Error()),
		))
	}

	mu.Lock()
	sentryOn := sentryEnabled
	mu.Unlock()

	if sentryOn {
		addBreadcrumb(ctx, &sentry.Breadcrumb{
			Category:  "observx.refusal",
			Message:   err.Error(),
			Level:     sentry.LevelInfo,
			Timestamp: time.Now(),
		})
	}

	// Logged whether or not an exporter is configured: with Sentry on, a
	// breadcrumb alone is retrievable only if a LATER exception happens on
	// the same hub, and a refusal usually has no later exception. Refusals
	// are client-driven, so the line is rate-limited: over the budget it
	// is counted, and the count is logged with the next line written.
	refusalLog.printf("refused: %s", scrubLog(err.Error()))
}

// WithRequestScope gives the request carried by ctx its own Sentry hub,
// so the breadcrumbs [ReportEvent] and [ReportRefusal] record for it are
// attached to this request's events only. Entry points call it once per
// request; a ctx that already carries a hub is returned unchanged, so a
// nested entry point (an in-process call inside a gateway request) stays
// in its caller's request. Without Sentry it returns ctx unchanged.
func WithRequestScope(ctx context.Context) context.Context {
	mu.Lock()
	on := sentryEnabled
	mu.Unlock()
	if !on || sentry.HasHubOnContext(ctx) {
		return ctx
	}
	hub := sentry.CurrentHub().Clone()
	hub.Scope().ClearBreadcrumbs()
	return sentry.SetHubOnContext(ctx, hub)
}

// addBreadcrumb records b on the request's own hub. A context without one
// gets no breadcrumb: the process-wide hub is shared by every concurrent
// request, so a breadcrumb there would be attached to whichever request
// next reports an error.
func addBreadcrumb(ctx context.Context, b *sentry.Breadcrumb) {
	if hub := sentry.GetHubFromContext(ctx); hub != nil {
		hub.AddBreadcrumb(b, nil)
	}
}

// Log budgets per channel: a burst, then a steady rate. Beyond it lines
// are counted, not written.
const (
	logBurst = 50
	logRate  = 10 // lines per second
)

var (
	refusalLog = newLogLimiter("refused", logBurst, logRate)
	errorLog   = newLogLimiter("error", logBurst, logRate)
	eventLog   = newLogLimiter("debug", logBurst, logRate)
	limiters   = []*logLimiter{refusalLog, errorLog, eventLog}
)

// logLimiter is a token bucket in front of log.Printf for one channel.
type logLimiter struct {
	name  string
	burst float64
	rate  float64
	now   func() time.Time

	mu         sync.Mutex
	tokens     float64
	last       time.Time
	suppressed int
	since      time.Time
}

func newLogLimiter(name string, burst, rate float64) *logLimiter {
	return &logLimiter{name: name, burst: burst, rate: rate, tokens: burst, now: time.Now}
}

// printf writes the line when the budget allows. A line that follows
// suppressed ones is preceded by their count.
func (l *logLimiter) printf(format string, args ...any) {
	l.mu.Lock()
	now := l.now()
	if !l.last.IsZero() {
		l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	}
	l.last = now
	if l.tokens < 1 {
		if l.suppressed == 0 {
			l.since = now
		}
		l.suppressed++
		l.mu.Unlock()
		return
	}
	l.tokens--
	n, since := l.suppressed, l.since
	l.suppressed = 0
	l.mu.Unlock()
	if n > 0 {
		log.Printf("observx: %d %q line(s) suppressed by the log rate limit since %s", n, l.name, since.Format(time.RFC3339))
	}
	log.Printf(format, args...)
}

// flush logs the count of lines suppressed since the last written one.
func (l *logLimiter) flush() {
	l.mu.Lock()
	n, since := l.suppressed, l.since
	l.suppressed = 0
	l.mu.Unlock()
	if n > 0 {
		log.Printf("observx: %d %q line(s) suppressed by the log rate limit since %s", n, l.name, since.Format(time.RFC3339))
	}
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
