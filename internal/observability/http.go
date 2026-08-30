package observability

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TraceMiddleware(
	tracer trace.Tracer,
	propagator propagation.TextMapPropagator,
) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		parent := propagator.Extract(
			ctx.Request.Context(),
			propagation.HeaderCarrier(ctx.Request.Header),
		)
		spanContext, span := tracer.Start(parent, ctx.Request.Method+" "+ctx.Request.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
		)
		ctx.Request = ctx.Request.WithContext(spanContext)
		defer func() {
			route := routeName(ctx)
			span.SetName(ctx.Request.Method + " " + route)
			span.SetAttributes(
				attribute.String("http.request.method", ctx.Request.Method),
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", ctx.Writer.Status()),
			)
			if ctx.Writer.Status() >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(ctx.Writer.Status()))
			}
			span.End()
		}()
		ctx.Next()
	}
}

func RequestMiddleware(
	metrics *Metrics,
	logger *slog.Logger,
	actorID string,
) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		startedAt := time.Now()
		requestID := strings.TrimSpace(ctx.GetHeader("X-Request-ID"))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		ctx.Header("X-Request-ID", requestID)
		defer func() {
			duration := time.Since(startedAt)
			route := routeName(ctx)
			metrics.RecordHTTPRequest(ctx.Request.Method, route, ctx.Writer.Status(), duration)
			spanContext := trace.SpanContextFromContext(ctx.Request.Context())
			logger.InfoContext(ctx.Request.Context(), "HTTP 请求完成",
				"request_id", requestID,
				"method", ctx.Request.Method,
				"route", route,
				"status", ctx.Writer.Status(),
				"duration_ms", duration.Milliseconds(),
				"actor_id", actorID,
				"project_id", ctx.Param("projectId"),
				"idempotency_key", boundedHeader(ctx.GetHeader("Idempotency-Key"), 128),
				"trace_id", spanContext.TraceID().String(),
			)
		}()
		ctx.Next()
	}
}

func routeName(ctx *gin.Context) string {
	if route := ctx.FullPath(); route != "" {
		return route
	}
	return "unmatched"
}

func boundedHeader(value string, maximumLength int) string {
	runes := []rune(value)
	if len(runes) <= maximumLength {
		return value
	}
	return string(runes[:maximumLength])
}
