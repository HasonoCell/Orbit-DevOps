package observability

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type ReleaseOperationPendingCounter func(context.Context) (int, error)

// ReleaseOperationSnapshotReader 读取由数据库权威事实重建的 ReleaseOperation 指标快照。
type ReleaseOperationSnapshotReader func(context.Context) (releaseoperation.MetricsSnapshot, error)

type Metrics struct {
	registry                     *prometheus.Registry
	httpRequests                 *prometheus.CounterVec
	httpDuration                 *prometheus.HistogramVec
	releaseOperationDuration     *prometheus.HistogramVec
	releaseOperationPhase        *prometheus.HistogramVec
	releaseOperationTerminal     *prometheus.CounterVec
	kubernetesReadFail           prometheus.Counter
	releaseOperationStatus       *prometheus.GaugeVec
	pendingReleaseOperationState *prometheus.GaugeVec
	releaseOperationEvents       *prometheus.GaugeVec
	releaseAttemptErrors         *prometheus.GaugeVec
	authorizationDeny            *prometheus.CounterVec
	idempotencyConflict          *prometheus.CounterVec
	pendingOnce                  sync.Once
	releaseOperationOnce         sync.Once
	refreshMu                    sync.Mutex
	releaseOperationSnapshot     ReleaseOperationSnapshotReader
}

func NewMetrics(pending ReleaseOperationPendingCounter) *Metrics {
	metrics := &Metrics{
		registry: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbit_devops",
			Name:      "http_requests_total",
			Help:      "Orbit-DevOps HTTP 请求总数。",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orbit_devops",
			Name:      "http_request_duration_seconds",
			Help:      "Orbit-DevOps HTTP 请求耗时。",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "route"}),
		releaseOperationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orbit_devops",
			Name:      "release_operation_duration_seconds",
			Help:      "Orbit-DevOps ReleaseOperation 尝试耗时。",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12),
		}, []string{"status", "category"}),
		releaseOperationPhase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orbit_devops",
			Name:      "release_operation_phase_duration_seconds",
			Help:      "ReleaseOperation 领取、执行、恢复与端到端阶段耗时。",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 18),
		}, []string{"phase"}),
		releaseOperationTerminal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbit_devops",
			Name:      "release_operation_terminal_total",
			Help:      "Orbit-DevOps ReleaseOperation 终态分类总数。",
		}, []string{"status", "category"}),
		kubernetesReadFail: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orbit_devops",
			Name:      "kubernetes_read_failures_total",
			Help:      "Orbit-DevOps Kubernetes 回读失败总数。",
		}),
		releaseOperationStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbit_devops", Name: "release_operation_status", Help: "按状态统计的 ReleaseOperation 当前数量。",
		}, []string{"status"}),
		pendingReleaseOperationState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbit_devops", Name: "pending_release_operation_state", Help: "按可领取性统计的 pending ReleaseOperation 数量。",
		}, []string{"availability"}),
		releaseOperationEvents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbit_devops", Name: "release_operation_events", Help: "PostgreSQL 审计中持久化的 ReleaseOperation 事件累计数量。",
		}, []string{"event"}),
		releaseAttemptErrors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "orbit_devops", Name: "release_attempt_errors", Help: "按稳定错误代码统计的 Attempt 累计数量。",
		}, []string{"error_code"}),
		authorizationDeny: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbit_devops", Name: "authorization_denials_total", Help: "按受控原因统计的项目授权拒绝数量。",
		}, []string{"reason"}),
		idempotencyConflict: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbit_devops", Name: "idempotency_conflicts_total", Help: "按命令类型统计的幂等指纹冲突数量。",
		}, []string{"command"}),
	}
	metrics.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.httpRequests,
		metrics.httpDuration,
		metrics.releaseOperationDuration,
		metrics.releaseOperationPhase,
		metrics.releaseOperationTerminal,
		metrics.kubernetesReadFail,
		metrics.releaseOperationStatus,
		metrics.pendingReleaseOperationState,
		metrics.releaseOperationEvents,
		metrics.releaseAttemptErrors,
		metrics.authorizationDeny,
		metrics.idempotencyConflict,
	)
	metrics.RegisterReleaseOperationPending(pending)
	return metrics
}

// RegisterReleaseOperations 注册数据库快照读取器；同一个 Metrics 实例只绑定一个运行时。
func (m *Metrics) RegisterReleaseOperations(reader ReleaseOperationSnapshotReader) {
	if reader == nil {
		return
	}
	m.releaseOperationOnce.Do(func() { m.releaseOperationSnapshot = reader })
}

func (m *Metrics) RegisterReleaseOperationPending(pending ReleaseOperationPendingCounter) {
	if pending == nil {
		return
	}
	m.pendingOnce.Do(func() {
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "orbit_devops",
			Name:      "pending_release_operations",
			Help:      "当前等待 Worker 领取的 ReleaseOperation 数量。",
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
		m.refreshReleaseOperationMetrics(request.Context())
		handler.ServeHTTP(response, request)
	})
}

// RegisterCollector 在启动阶段接入可选运行模块的指标，不让核心观测层依赖队列实现。
func (m *Metrics) RegisterCollector(collector prometheus.Collector) {
	m.registry.MustRegister(collector)
}

func (m *Metrics) RecordHTTPRequest(method string, route string, status int, duration time.Duration) {
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

func (m *Metrics) RecordReleaseOperation(status string, category string, duration time.Duration) {
	m.releaseOperationDuration.WithLabelValues(status, category).Observe(duration.Seconds())
	m.releaseOperationTerminal.WithLabelValues(status, category).Inc()
}

// RecordReleaseOperationPhase 记录固定阶段集合的耗时，调用方不得把动态值作为 phase。
func (m *Metrics) RecordReleaseOperationPhase(phase string, duration time.Duration) {
	m.releaseOperationPhase.WithLabelValues(phase).Observe(duration.Seconds())
}

func (m *Metrics) RecordKubernetesReadFailure() {
	m.kubernetesReadFail.Inc()
}

// RecordAuthorizationDenial 记录受控拒绝类别，不接收 Actor 或资源标识。
func (m *Metrics) RecordAuthorizationDenial(reason string) {
	m.authorizationDeny.WithLabelValues(reason).Inc()
}

// RecordIdempotencyConflict 按稳定命令类型记录指纹冲突。
func (m *Metrics) RecordIdempotencyConflict(commandType string) {
	m.idempotencyConflict.WithLabelValues(commandType).Inc()
}

func (m *Metrics) refreshReleaseOperationMetrics(ctx context.Context) {
	if m.releaseOperationSnapshot == nil {
		return
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	readContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	snapshot, err := m.releaseOperationSnapshot(readContext)
	if err != nil {
		return
	}
	m.releaseOperationStatus.Reset()
	for _, count := range snapshot.Statuses {
		m.releaseOperationStatus.WithLabelValues(count.Label).Set(float64(count.Count))
	}
	m.pendingReleaseOperationState.Reset()
	m.pendingReleaseOperationState.WithLabelValues("available").Set(float64(snapshot.PendingAvailable))
	m.pendingReleaseOperationState.WithLabelValues("delayed").Set(float64(snapshot.PendingDelayed))
	m.releaseOperationEvents.Reset()
	for _, count := range snapshot.Events {
		m.releaseOperationEvents.WithLabelValues(count.Label).Set(float64(count.Count))
	}
	m.releaseAttemptErrors.Reset()
	for _, count := range snapshot.AttemptErrorCodes {
		m.releaseAttemptErrors.WithLabelValues(count.Label).Set(float64(count.Count))
	}
}
