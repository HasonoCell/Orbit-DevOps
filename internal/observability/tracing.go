package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Tracing struct {
	Provider   *sdktrace.TracerProvider
	Propagator propagation.TextMapPropagator
}

func NewTracing(logger *slog.Logger) Tracing {
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	}
	if logger != nil {
		options = append(options, sdktrace.WithSyncer(slogSpanExporter{logger: logger}))
	}
	return Tracing{
		Provider: sdktrace.NewTracerProvider(options...),
		Propagator: propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	}
}

func (t Tracing) Shutdown(ctx context.Context) error {
	return t.Provider.Shutdown(ctx)
}

type slogSpanExporter struct {
	logger *slog.Logger
}

func (e slogSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, span := range spans {
		e.logger.InfoContext(ctx, "Trace Span 完成",
			"span_name", span.Name(),
			"trace_id", span.SpanContext().TraceID(),
			"span_id", span.SpanContext().SpanID(),
			"parent_span_id", span.Parent().SpanID(),
			"duration_ms", span.EndTime().Sub(span.StartTime()).Milliseconds(),
			"status", span.Status().Code.String(),
		)
	}
	return nil
}

func (e slogSpanExporter) Shutdown(context.Context) error {
	return nil
}
