package observability

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TraceConfig 只描述导出与采样边界；认证 Headers 由官方 exporter 从运行环境读取，不持久化。
type TraceConfig struct {
	Exporter, Endpoint, Protocol   string
	SampleRatio                    float64
	ExportTimeout, ShutdownTimeout time.Duration
}

func DefaultTraceConfig() TraceConfig {
	return TraceConfig{Exporter: "slog", Protocol: "http/protobuf", SampleRatio: 1,
		ExportTimeout: 2 * time.Second, ShutdownTimeout: 5 * time.Second}
}
func (c TraceConfig) Validate() error {
	if (c.Exporter != "off" && c.Exporter != "slog" && c.Exporter != "otlp") ||
		math.IsNaN(c.SampleRatio) || math.IsInf(c.SampleRatio, 0) || c.SampleRatio < 0 || c.SampleRatio > 1 ||
		c.ExportTimeout <= 0 || c.ShutdownTimeout <= 0 || c.Protocol != "http/protobuf" {
		return errors.New("invalid trace configuration")
	}
	if c.Exporter != "otlp" {
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid trace endpoint")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme == "http" && !loopback {
		return errors.New("trace endpoint requires HTTPS outside loopback")
	}
	return nil
}

type Tracing struct {
	Provider        trace.TracerProvider
	Propagator      propagation.TextMapPropagator
	ExportFailures  prometheus.Counter
	shutdown        func(context.Context) error
	shutdownTimeout time.Duration
}

// NewTracing 采用 SDK 非阻塞有界批处理；接收器暂时离线不会阻止业务进程启动。
func NewTracing(ctx context.Context, config TraceConfig, logger *slog.Logger, process string) (Tracing, error) {
	if err := config.Validate(); err != nil {
		return Tracing{}, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tracing := Tracing{Provider: noop.NewTracerProvider(), Propagator: propagation.TraceContext{},
		ExportFailures: prometheus.NewCounter(prometheus.CounterOpts{Namespace: "orbit_devops",
			Name: "trace_export_failures_total", Help: "有界 Trace 导出失败次数。", ConstLabels: prometheus.Labels{"process": process}}),
		shutdown: func(context.Context) error { return nil }, shutdownTimeout: config.ShutdownTimeout}
	if config.Exporter == "off" {
		return tracing, nil
	}
	var exporter sdktrace.SpanExporter = slogSpanExporter{logger: logger}
	if config.Exporter == "otlp" {
		if err := validateOTLPEnvironment(); err != nil {
			return Tracing{}, err
		}
		// 不接受环境中的跳过证书验证，不跟随重定向转发认证 Headers。
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		client := &http.Client{Transport: transport, Timeout: config.ExportTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		otlp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(config.Endpoint),
			otlptracehttp.WithEncoding(otlptracehttp.EncodingProtobuf), otlptracehttp.WithHTTPClient(client),
			otlptracehttp.WithTimeout(config.ExportTimeout),
			otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: true, InitialInterval: 100 * time.Millisecond,
				MaxInterval: 500 * time.Millisecond, MaxElapsedTime: config.ExportTimeout}))
		if err != nil {
			transport.CloseIdleConnections()
			return Tracing{}, errors.New("trace_exporter_initialization_failed")
		}
		exporter = httpSpanExporter{SpanExporter: otlp, transport: transport}
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "orbit-devops-"+process))),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(config.SampleRatio))),
		sdktrace.WithBatcher(safeSpanExporter{exporter: exporter, failures: tracing.ExportFailures, logger: logger,
			timeout: config.ExportTimeout}, sdktrace.WithMaxQueueSize(1024), sdktrace.WithMaxExportBatchSize(128),
			sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(config.ExportTimeout)))
	tracing.Provider, tracing.shutdown = provider, provider.Shutdown
	return tracing, nil
}

// Shutdown 使用自己的退出预算；调用者须在业务处理器结束后、数据库关闭前执行。
func (t Tracing) Shutdown(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, t.shutdownTimeout)
	defer cancel()
	if t.shutdown(bounded) != nil {
		return errors.New("trace_shutdown_incomplete")
	}
	return nil
}

// safeSpanExporter 只返回稳定错误，官方 SDK 的错误出口也不会拿到原始连接材料。
type safeSpanExporter struct {
	exporter sdktrace.SpanExporter
	failures prometheus.Counter
	logger   *slog.Logger
	timeout  time.Duration
}

func (e safeSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	bounded, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	if e.exporter.ExportSpans(bounded, spans) != nil {
		e.failures.Inc()
		e.logger.WarnContext(ctx, "Trace 导出暂时不可用")
		return errors.New("trace_export_failed")
	}
	return nil
}
func (e safeSpanExporter) Shutdown(ctx context.Context) error {
	if e.exporter.Shutdown(ctx) != nil {
		return errors.New("trace_exporter_shutdown_failed")
	}
	return nil
}

type httpSpanExporter struct {
	sdktrace.SpanExporter
	transport *http.Transport
}

func (e httpSpanExporter) Shutdown(ctx context.Context) error {
	defer e.transport.CloseIdleConnections()
	return e.SpanExporter.Shutdown(ctx)
}

type slogSpanExporter struct{ logger *slog.Logger }

func (e slogSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, span := range spans {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.logger.InfoContext(ctx, "Trace Span 完成", "span_name", span.Name(), "trace_id", span.SpanContext().TraceID(),
			"span_id", span.SpanContext().SpanID(), "parent_span_id", span.Parent().SpanID(),
			"duration_ms", span.EndTime().Sub(span.StartTime()).Milliseconds(), "status", span.Status().Code.String())
	}
	return nil
}
func (slogSpanExporter) Shutdown(context.Context) error { return nil }
