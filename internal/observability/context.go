package observability

import (
	"context"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceFields 只持久化 W3C Trace Context，不携带 Baggage 或认证材料。
func TraceFields(ctx context.Context) (string, string) {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent"), carrier.Get("tracestate")
}

// RestoreTrace 仅接受持久化父节点。缺少或损坏的旧数据创建新根，不继承运输进程偶然带入的 Span。
func RestoreTrace(ctx context.Context, parent, state string, propagator propagation.TextMapPropagator) context.Context {
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": parent, "tracestate": state})
}
