package observability

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type PendingCounter func(context.Context) (int, error)
type OperationSnapshotReader func(context.Context) (operation.MetricsSnapshot, error)

type Metrics struct {
	registry            *prometheus.Registry
	httpRequests        *prometheus.CounterVec
	httpDuration        *prometheus.HistogramVec
	operationDuration   *prometheus.HistogramVec
	operationPhase      *prometheus.HistogramVec
	operationTerminal   *prometheus.CounterVec
	kubernetesReadFail  prometheus.Counter
	operationStatus     *prometheus.GaugeVec
	pendingState        *prometheus.GaugeVec
	operationEvents     *prometheus.GaugeVec
	attemptErrors       *prometheus.GaugeVec
	authorizationDeny   *prometheus.CounterVec
	idempotencyConflict *prometheus.CounterVec
	pendingOnce         sync.Once
	operationOnce       sync.Once
	refreshMu           sync.Mutex
	operationSnapshot   OperationSnapshotReader
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
		operationPhase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orbitops",
			Name:      "operation_phase_duration_seconds",
			Help:      "Operation 领取、执行、恢复与端到端阶段耗时。",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 18),
		}, []string{"phase"}),
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
		operationStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbitops", Name: "operation_status", Help: "按状态统计的 Operation 当前数量。",
		}, []string{"status"}),
		pendingState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbitops", Name: "pending_operation_state", Help: "按可领取性统计的 pending Operation 数量。",
		}, []string{"availability"}),
		operationEvents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbitops", Name: "operation_events", Help: "PostgreSQL 审计中持久化的 Operation 事件累计数量。",
		}, []string{"event"}),
		attemptErrors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbitops", Name: "attempt_errors", Help: "按稳定错误代码统计的 Attempt 累计数量。",
		}, []string{"error_code"}),
		authorizationDeny: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbitops", Name: "authorization_denials_total", Help: "按受控原因统计的项目授权拒绝数量。",
		}, []string{"reason"}),
		idempotencyConflict: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbitops", Name: "idempotency_conflicts_total", Help: "按命令类型统计的幂等指纹冲突数量。",
		}, []string{"command"}),
	}
	metrics.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.httpRequests,
		metrics.httpDuration,
		metrics.operationDuration,
		metrics.operationPhase,
		metrics.operationTerminal,
		metrics.kubernetesReadFail,
		metrics.operationStatus,
		metrics.pendingState,
		metrics.operationEvents,
		metrics.attemptErrors,
		metrics.authorizationDeny,
		metrics.idempotencyConflict,
	)
	metrics.RegisterPending(pending)
	return metrics
}

func (m *Metrics) RegisterOperations(reader OperationSnapshotReader) {
	if reader == nil {
		return
	}
	m.operationOnce.Do(func() { m.operationSnapshot = reader })
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
	handler := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		m.refreshOperationMetrics(request.Context())
		handler.ServeHTTP(response, request)
	})
}

func (m *Metrics) RecordHTTPRequest(method string, route string, status int, duration time.Duration) {
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

func (m *Metrics) RecordOperation(status string, category string, duration time.Duration) {
	m.operationDuration.WithLabelValues(status, category).Observe(duration.Seconds())
	m.operationTerminal.WithLabelValues(status, category).Inc()
}

func (m *Metrics) RecordOperationPhase(phase string, duration time.Duration) {
	m.operationPhase.WithLabelValues(phase).Observe(duration.Seconds())
}

func (m *Metrics) RecordKubernetesReadFailure() {
	m.kubernetesReadFail.Inc()
}

func (m *Metrics) RecordAuthorizationDenial(reason string) {
	m.authorizationDeny.WithLabelValues(reason).Inc()
}

func (m *Metrics) RecordIdempotencyConflict(commandType string) {
	m.idempotencyConflict.WithLabelValues(commandType).Inc()
}

func (m *Metrics) refreshOperationMetrics(ctx context.Context) {
	if m.operationSnapshot == nil {
		return
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	readContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	snapshot, err := m.operationSnapshot(readContext)
	if err != nil {
		return
	}
	m.operationStatus.Reset()
	for _, count := range snapshot.Statuses {
		m.operationStatus.WithLabelValues(count.Label).Set(float64(count.Count))
	}
	m.pendingState.Reset()
	m.pendingState.WithLabelValues("available").Set(float64(snapshot.PendingAvailable))
	m.pendingState.WithLabelValues("delayed").Set(float64(snapshot.PendingDelayed))
	m.operationEvents.Reset()
	for _, count := range snapshot.Events {
		m.operationEvents.WithLabelValues(count.Label).Set(float64(count.Count))
	}
	m.attemptErrors.Reset()
	for _, count := range snapshot.AttemptErrorCodes {
		m.attemptErrors.WithLabelValues(count.Label).Set(float64(count.Count))
	}
}
