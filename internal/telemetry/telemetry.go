// Package telemetry wires OpenTelemetry traces, metrics, and logs for llmtap
// itself ("self-instrumentation") and exposes the meters used to record
// GenAI signals on every proxied call.
//
// The Setup function returns a single Shutdown closure that flushes every
// provider in reverse order of construction and is safe to call exactly once.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/colinedwardwood/llmtap/internal/buildinfo"
	"github.com/colinedwardwood/llmtap/internal/config"
	"github.com/colinedwardwood/llmtap/internal/genai"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	otelloggl "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// Providers carries everything callers need post-Setup. The Shutdown closure
// is the *only* lifecycle handle the caller has to manage.
type Providers struct {
	Tracer trace.Tracer
	Meter  metric.Meter
	// Logger is reserved for content.mode:logs (D31): content attached
	// via the OTel log signal rather than span events. Held here so
	// Setup constructs the log pipeline once; unused by the hot path
	// until that mode lands. Do not delete as "dead" — the logger
	// provider's Shutdown still depends on Setup having built it.
	Logger   otellog.Logger
	Meters   GenAIMeters
	Shutdown func(context.Context) error
	// Ready reports whether the telemetry pipeline is ready to serve
	// traffic. The /readyz endpoint surfaces this. A nil Ready is
	// treated as "always ready" by the proxy (tests). Production
	// Setup returns Gate.Ready: true only after the first successful
	// OTLP export and while not draining.
	Ready func() bool
	// BeginDrain flips Ready false for rolling updates. Call as soon
	// as SIGTERM is observed, before graceful handler drain. Nil is
	// a no-op (tests).
	BeginDrain func()
}

// GenAIMeters bundles the OTel GenAI instruments. Pre-creating them at
// startup lets the hot path stay allocation-free.
type GenAIMeters struct {
	TokenUsage        metric.Int64Histogram
	OperationDuration metric.Float64Histogram
	TimeToFirstToken  metric.Float64Histogram
	TimeToFirstByte   metric.Float64Histogram
	// CostUSD is the per-call cost histogram. Dashboards reach p50/p95
	// cost per operation via this; PromQL `_sum` series gives the
	// running spend if the float-counter drift on .Total isn't a
	// concern.
	CostUSD metric.Float64Histogram
	// CostUSDTotal is the monotonic cumulative-spend counter. Use this
	// when a single integer-cumulative metric is preferable to the
	// histogram's _sum (e.g. when downsampling or recording rules
	// would lose the count series).
	CostUSDTotal metric.Float64Counter
}

// Setup builds traces+metrics+logs providers, registers them globally so
// instrumented libraries (otelhttp, otelslog) pick them up, and returns the
// instruments llmtap will record into.
//
// slogLogger, when non-nil, receives OTel SDK / exporter errors via
// otel.SetErrorHandler and increments llmtap.telemetry.export_errors.total
// so a silent collector outage is visible on the FinOps dashboard.
//
// The returned Shutdown is composable: it flushes every provider, joins their
// errors, and respects ctx for the deadline.
func Setup(ctx context.Context, cfg config.Config, slogLogger *slog.Logger) (Providers, error) {
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return Providers{}, fmt.Errorf("resource: %w", err)
	}

	gate := NewGate()

	tp, tShut, err := newTracerProvider(ctx, cfg, res, gate)
	if err != nil {
		return Providers{}, fmt.Errorf("tracer provider: %w", err)
	}
	mp, mShut, err := newMeterProvider(ctx, cfg, res, gate)
	if err != nil {
		_ = tShut(ctx)
		return Providers{}, fmt.Errorf("meter provider: %w", err)
	}
	lp, lShut, err := newLoggerProvider(ctx, cfg, res)
	if err != nil {
		_ = mShut(ctx)
		_ = tShut(ctx)
		return Providers{}, fmt.Errorf("logger provider: %w", err)
	}

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otelloggl.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	tracer := tp.Tracer("github.com/colinedwardwood/llmtap",
		trace.WithSchemaURL(semconv.SchemaURL))
	meter := mp.Meter("github.com/colinedwardwood/llmtap",
		metric.WithSchemaURL(semconv.SchemaURL))
	logger := lp.Logger("github.com/colinedwardwood/llmtap",
		otellog.WithSchemaURL(semconv.SchemaURL))

	meters, err := NewGenAIMeters(meter)
	if err != nil {
		_ = lShut(ctx)
		_ = mShut(ctx)
		_ = tShut(ctx)
		return Providers{}, fmt.Errorf("instruments: %w", err)
	}

	exportErrors, err := meter.Int64Counter(
		"llmtap.telemetry.export_errors.total",
		metric.WithDescription("OpenTelemetry SDK / exporter errors (dropped batches, unreachable collector). Incremented from otel.SetErrorHandler."),
	)
	if err != nil {
		_ = lShut(ctx)
		_ = mShut(ctx)
		_ = tShut(ctx)
		return Providers{}, fmt.Errorf("export errors counter: %w", err)
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		exportErrors.Add(context.Background(), 1)
		if slogLogger != nil {
			slogLogger.Warn("otel SDK error", slog.Any("err", err))
		}
	}))

	shutdown := func(ctx context.Context) error {
		return errors.Join(lShut(ctx), mShut(ctx), tShut(ctx))
	}

	// Kick an early flush so /readyz can go green within seconds of
	// a healthy collector instead of waiting for the 15s metric
	// interval. Failures leave the gate not-ready — correct.
	// WithoutCancel: the flush must not abort just because Setup's
	// caller cancelled a short deadline; SIGTERM drain still has
	// BeginDrain for readiness.
	go func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tp.ForceFlush(flushCtx)
		_ = mp.ForceFlush(flushCtx)
	}()

	return Providers{
		Tracer:     tracer,
		Meter:      meter,
		Logger:     logger,
		Meters:     meters,
		Shutdown:   shutdown,
		Ready:      gate.Ready,
		BeginDrain: gate.BeginDrain,
	}, nil
}

func buildResource(ctx context.Context, cfg config.Config) (*resource.Resource, error) {
	version, _, _ := buildinfo.Resolve()
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.Service.Name),
			semconv.ServiceNamespace(cfg.Service.Namespace),
			semconv.ServiceVersion(version),
			semconv.DeploymentEnvironmentName(cfg.Service.Env),
		),
	)
}

type shutdownFn func(context.Context) error

func newTracerProvider(ctx context.Context, cfg config.Config, res *resource.Resource, gate *Gate) (*sdktrace.TracerProvider, shutdownFn, error) {
	exp, err := newTraceExporter(ctx, cfg.Telemetry)
	if err != nil {
		return nil, nil, err
	}
	// Record first, decide at export time. Error, latency, and computed
	// cost do not exist at span start, so a head sampler cannot preserve
	// the signals operators most need.
	sampler := sdktrace.AlwaysSample()
	limits := sdktrace.NewSpanLimits()
	// Bound every span attribute (and event attribute) value. Captured
	// completion text is separately capped at the provider layer
	// (provider.maxAssembledBytes, same figure); this limit is the
	// backstop that keeps ANY attribute from producing multi-hundred-KB
	// spans that OTLP collectors truncate or reject wholesale.
	limits.AttributeValueLengthLimit = 64 * 1024
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(&exportOKSpanExporter{SpanExporter: &prioritySpanExporter{
			SpanExporter:  exp,
			ratio:         cfg.Telemetry.SampleRatio,
			costThreshold: cfg.Telemetry.CostSampleThresholdUSD,
			slowThreshold: cfg.Telemetry.SlowSampleThreshold,
		}, gate: gate},
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
		sdktrace.WithRawSpanLimits(limits),
	)
	return tp, tp.Shutdown, nil
}

func newTraceExporter(ctx context.Context, t config.Telemetry) (sdktrace.SpanExporter, error) {
	switch t.Protocol {
	case config.ProtoGRPC:
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(t.Endpoint), otlptracegrpc.WithTimeout(t.Timeout)}
		if t.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if len(t.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(t.Headers))
		}
		return otlptrace.New(ctx, otlptracegrpc.NewClient(opts...))
	case config.ProtoHTTP:
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(t.Endpoint), otlptracehttp.WithTimeout(t.Timeout)}
		if t.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(t.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(t.Headers))
		}
		return otlptrace.New(ctx, otlptracehttp.NewClient(opts...))
	default:
		return nil, fmt.Errorf("unknown protocol %q", t.Protocol)
	}
}

func newMeterProvider(ctx context.Context, cfg config.Config, res *resource.Resource, gate *Gate) (*sdkmetric.MeterProvider, shutdownFn, error) {
	exp, err := newMetricExporter(ctx, cfg.Telemetry)
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(
			&exportOKMetricExporter{Exporter: exp, gate: gate},
			sdkmetric.WithInterval(15*time.Second),
		)),
	)
	return mp, mp.Shutdown, nil
}

func newMetricExporter(ctx context.Context, t config.Telemetry) (sdkmetric.Exporter, error) {
	switch t.Protocol {
	case config.ProtoGRPC:
		opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(t.Endpoint), otlpmetricgrpc.WithTimeout(t.Timeout)}
		if t.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		if len(t.Headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(t.Headers))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	case config.ProtoHTTP:
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(t.Endpoint), otlpmetrichttp.WithTimeout(t.Timeout)}
		if t.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		if len(t.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(t.Headers))
		}
		return otlpmetrichttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown protocol %q", t.Protocol)
	}
}

func newLoggerProvider(ctx context.Context, cfg config.Config, res *resource.Resource) (*sdklog.LoggerProvider, shutdownFn, error) {
	exp, err := newLogExporter(ctx, cfg.Telemetry)
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	return lp, lp.Shutdown, nil
}

func newLogExporter(ctx context.Context, t config.Telemetry) (sdklog.Exporter, error) {
	switch t.Protocol {
	case config.ProtoGRPC:
		opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(t.Endpoint), otlploggrpc.WithTimeout(t.Timeout)}
		if t.Insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		if len(t.Headers) > 0 {
			opts = append(opts, otlploggrpc.WithHeaders(t.Headers))
		}
		return otlploggrpc.New(ctx, opts...)
	case config.ProtoHTTP:
		opts := []otlploghttp.Option{otlploghttp.WithEndpoint(t.Endpoint), otlploghttp.WithTimeout(t.Timeout)}
		if t.Insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
		if len(t.Headers) > 0 {
			opts = append(opts, otlploghttp.WithHeaders(t.Headers))
		}
		return otlploghttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown protocol %q", t.Protocol)
	}
}

func NewGenAIMeters(m metric.Meter) (GenAIMeters, error) {
	return newGenAIMeters(m)
}

func newGenAIMeters(m metric.Meter) (GenAIMeters, error) {
	tokens, err := m.Int64Histogram(
		genai.MetricTokenUsage,
		metric.WithUnit("{token}"),
		metric.WithDescription("Tokens used in a GenAI request, labelled by token type."),
	)
	if err != nil {
		return GenAIMeters{}, err
	}
	dur, err := m.Float64Histogram(
		genai.MetricOperationDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Wall-clock duration of a GenAI operation."),
	)
	if err != nil {
		return GenAIMeters{}, err
	}
	ttft, err := m.Float64Histogram(
		genai.MetricTimeToFirstToken,
		metric.WithUnit("s"),
		metric.WithDescription("Time from request start to first streamed token."),
	)
	if err != nil {
		return GenAIMeters{}, err
	}
	ttfb, err := m.Float64Histogram(
		genai.MetricTimeToFirstByte,
		metric.WithUnit("s"),
		metric.WithDescription("Time from request start to first upstream response byte (status/headers)."),
	)
	if err != nil {
		return GenAIMeters{}, err
	}
	// Per-call cost histogram. USD-scaled buckets span free-tier
	// embeddings ($0.00002 / call) through expensive single inferences
	// (~$100 / call for very long gpt-4-class contexts).
	// Unit is the UCUM annotation "{USD}", not the bare string "USD":
	// Prometheus's OTLP translator appends non-annotation units to the
	// metric name, and since it compares case-sensitively against the
	// existing "usd" name token, unit "USD" produced the mangled
	// gen_ai_client_cost_usd_USD_* series that no dashboard queried.
	// Annotation units are dropped by the translator, keeping the
	// Prometheus names exactly what the name says.
	cost, err := m.Float64Histogram(
		genai.MetricCostUSD,
		metric.WithUnit("{USD}"),
		metric.WithDescription("Distribution of per-call USD cost, computed from a configurable price table."),
		metric.WithExplicitBucketBoundaries(1e-5, 1e-4, 1e-3, 0.01, 0.1, 1, 10, 100),
	)
	if err != nil {
		return GenAIMeters{}, err
	}
	costTotal, err := m.Float64Counter(
		genai.MetricCostUSDTotal,
		metric.WithUnit("{USD}"),
		metric.WithDescription("Monotonic cumulative USD cost of GenAI requests. Sibling to the cost histogram; the counter avoids float drift on the histogram _sum across millions of small adds."),
	)
	if err != nil {
		return GenAIMeters{}, err
	}
	return GenAIMeters{
		TokenUsage:        tokens,
		OperationDuration: dur,
		TimeToFirstToken:  ttft,
		TimeToFirstByte:   ttfb,
		CostUSD:           cost,
		CostUSDTotal:      costTotal,
	}, nil
}
