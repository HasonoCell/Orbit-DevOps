package observability

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type PendingCounter func(context.Context) (int, error)

type Metrics struct {
	registry           *prometheus.Registry
	httpRequests       *prometheus.CounterVec
	httpDuration       *prometheus.HistogramVec
	operationDuration  *prometheus.HistogramVec
	operationTerminal  *prometheus.CounterVec
	kubernetesReadFail prometheus.Counter
	pendingOnce        sync.Once
}

func NewMetrics(pending PendingCounter) *Metrics {
	metrics := &Metrics{
		registry: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbitops",
			Name:      "http_requests_total",
			Help:      "OrbitOps HTTP 请求总数。",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orbitops",
			Name:      "http_request_duration_seconds",
			Help:      "OrbitOps HTTP 请求耗时。",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "route"}),
		operationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orbitops",
			Name:      "operation_duration_seconds",
			Help:      "OrbitOps Operation 尝试耗时。",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12),
		}, []string{"status", "category"}),
		operationTerminal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbitops",
			Name:      "operation_terminal_total",
			Help:      "OrbitOps Operation 终态分类总数。",
		}, []string{"status", "category"}),
		kubernetesReadFail: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orbitops",
			Name:      "kubernetes_read_failures_total",
			Help:      "OrbitOps Kubernetes 回读失败总数。",
		}),
	}
	metrics.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.httpRequests,
		metrics.httpDuration,
		metrics.operationDuration,
		metrics.operationTerminal,
		metrics.kubernetesReadFail,
	)
	metrics.RegisterPending(pending)
	return metrics
}

func (m *Metrics) RegisterPending(pending PendingCounter) {
	if pending == nil {
		return
	}
	m.pendingOnce.Do(func() {
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "orbitops",
			Name:      "pending_operations",
			Help:      "当前等待 Worker 领取的 Operation 数量。",
		}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			count, err := pending(ctx)
			if err != nil {
				return math.NaN()
			}
			return float64(count)
		}))
	})
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) RecordHTTPRequest(method string, route string, status int, duration time.Duration) {
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

func (m *Metrics) RecordOperation(status string, category string, duration time.Duration) {
	m.operationDuration.WithLabelValues(status, category).Observe(duration.Seconds())
	m.operationTerminal.WithLabelValues(status, category).Inc()
}

func (m *Metrics) RecordKubernetesReadFailure() {
	m.kubernetesReadFail.Inc()
}
