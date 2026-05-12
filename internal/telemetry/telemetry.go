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
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Providers carries everything callers need post-Setup. The Shutdown closure
// is the *only* lifecycle handle the caller has to manage.
type Providers struct {
	Tracer   trace.Tracer
	Meter    metric.Meter
	Logger   otellog.Logger
	Meters   GenAIMeters
	Shutdown func(context.Context) error
}

// GenAIMeters bundles the OTel GenAI instruments. Pre-creating them at
// startup lets the hot path stay allocation-free.
type GenAIMeters struct {
	TokenUsage        metric.Int64Histogram
	OperationDuration metric.Float64Histogram
	TimeToFirstToken  metric.Float64Histogram
	CostUSD           metric.Float64Counter
}

// Setup builds traces+metrics+logs providers, registers them globally so
// instrumented libraries (otelhttp, otelslog) pick them up, and returns the
// instruments llmtap will record into.
//
// The returned Shutdown is composable: it flushes every provider, joins their
// errors, and respects ctx for the deadline.
func Setup(ctx context.Context, cfg config.Config) (Providers, error) {
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return Providers{}, fmt.Errorf("resource: %w", err)
	}

	tp, tShut, err := newTracerProvider(ctx, cfg, res)
	if err != nil {
		return Providers{}, fmt.Errorf("tracer provider: %w", err)
	}
	mp, mShut, err := newMeterProvider(ctx, cfg, res)
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
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	tracer := tp.Tracer("github.com/colinedwardwood/llmtap")
	meter := mp.Meter("github.com/colinedwardwood/llmtap")
	logger := lp.Logger("github.com/colinedwardwood/llmtap")

	meters, err := newGenAIMeters(meter)
	if err != nil {
		_ = lShut(ctx)
		_ = mShut(ctx)
		_ = tShut(ctx)
		return Providers{}, fmt.Errorf("instruments: %w", err)
	}

	shutdown := func(ctx context.Context) error {
		return errors.Join(lShut(ctx), mShut(ctx), tShut(ctx))
	}

	return Providers{
		Tracer:   tracer,
		Meter:    meter,
		Logger:   logger,
		Meters:   meters,
		Shutdown: shutdown,
	}, nil
}

func buildResource(ctx context.Context, cfg config.Config) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.Service.Name),
			semconv.ServiceNamespace(cfg.Service.Namespace),
			semconv.ServiceVersion(buildinfo.Version),
			semconv.DeploymentEnvironment(cfg.Service.Env),
		),
	)
}

type shutdownFn func(context.Context) error

func newTracerProvider(ctx context.Context, cfg config.Config, res *resource.Resource) (*sdktrace.TracerProvider, shutdownFn, error) {
	exp, err := newTraceExporter(ctx, cfg.Telemetry)
	if err != nil {
		return nil, nil, err
	}
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Telemetry.SampleRatio))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
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

func newMeterProvider(ctx context.Context, cfg config.Config, res *resource.Resource) (*sdkmetric.MeterProvider, shutdownFn, error) {
	exp, err := newMetricExporter(ctx, cfg.Telemetry)
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
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
	cost, err := m.Float64Counter(
		genai.MetricCostUSD,
		metric.WithUnit("USD"),
		metric.WithDescription("Estimated USD cost of GenAI requests, computed from a configurable price table."),
	)
	if err != nil {
		return GenAIMeters{}, err
	}
	return GenAIMeters{
		TokenUsage:        tokens,
		OperationDuration: dur,
		TimeToFirstToken:  ttft,
		CostUSD:           cost,
	}, nil
}
