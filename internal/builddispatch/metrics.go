package builddispatch

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Ready 要求数据库、Redis 与两个维护循环都可用；单次构建失败不影响进程就绪。
func (s *Service) Ready(ctx context.Context) error {
	if !s.running.Load() || time.Since(time.Unix(0, s.lastPublish.Load())) > max(3*s.config.PollInterval, 10*time.Second) ||
		time.Since(time.Unix(0, s.lastRepair.Load())) > max(3*s.config.RepairInterval, 10*time.Second) {
		return errors.New("build_dispatch_loops_unavailable")
	}
	if _, err := s.operations.ReadMetricsSnapshot(ctx); err != nil {
		return errors.New("build_dispatch_store_unavailable")
	}
	if err := s.connection.Ping(ctx).Err(); err != nil {
		return errors.New("build_queue_unavailable")
	}
	return nil
}

func (s *Service) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		response.Header().Set("Content-Type", "application/json")
		if s.Ready(ctx) != nil {
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = response.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		_, _ = response.Write([]byte(`{"status":"ok"}`))
	})
}

var buildDescriptors = []*prometheus.Desc{
	prometheus.NewDesc("orbit_devops_build_pending_available", "当前可以领取的 BuildOperation 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_pending_delayed", "当前等待退避的 BuildOperation 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_running", "当前运行中的 BuildOperation 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_attention_required", "当前需要人工处理的 BuildOperation 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_succeeded_total", "持久化的成功 BuildOperation 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_failed_total", "持久化的失败 BuildOperation 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_canceled_total", "持久化的取消 BuildOperation 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_active_executors", "已记录外部 Job 的运行 Attempt 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_attempt_duration_seconds", "已完成 BuildAttempt 的累计执行秒数。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_attempt_duration_count", "已完成 BuildAttempt 数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_pending", "当前有效待投递构建意图数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_published", "已入队但未取得业务执行权的构建意图数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_quarantined", "协议隔离的构建意图数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_oldest_age_seconds", "最老有效构建意图年龄。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_reservations", "构建运输领取累计次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_redeliveries", "同代次构建补发累计次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_send_errors_total", "本进程构建入队失败次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_received_total", "本进程收到的合法构建引用次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_ignored_total", "本进程忽略的过期或重复构建消息次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_invalid_total", "本进程丢弃的非法构建消息次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_execution_errors_total", "本进程构建消费基础设施错误次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_publish_success_timestamp_seconds", "投递循环最近成功时间。", nil, nil),
	prometheus.NewDesc("orbit_devops_build_dispatch_repair_success_timestamp_seconds", "补偿循环最近成功时间。", nil, nil),
}

func (s *Service) Describe(channel chan<- *prometheus.Desc) {
	for _, descriptor := range buildDescriptors {
		channel <- descriptor
	}
}

// Collect 组合可重建数据库事实和当前进程计数；数据库故障时对应指标显式为 NaN。
func (s *Service) Collect(channel chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := s.operations.ReadMetricsSnapshot(ctx)
	values := []float64{
		float64(snapshot.PendingAvailable), float64(snapshot.PendingDelayed), float64(snapshot.Running),
		float64(snapshot.AttentionRequired), float64(snapshot.Succeeded), float64(snapshot.Failed),
		float64(snapshot.Canceled), float64(snapshot.ActiveExecutors), snapshot.AttemptDurationSum,
		float64(snapshot.AttemptDurationCount), float64(snapshot.DispatchPending), float64(snapshot.DispatchPublished),
		float64(snapshot.DispatchQuarantined), snapshot.DispatchOldestAge, float64(snapshot.Reservations),
		float64(snapshot.Redeliveries), float64(s.sendErrors.Load()), float64(s.received.Load()),
		float64(s.ignored.Load()), float64(s.invalid.Load()), float64(s.executionErrors.Load()),
		float64(s.lastPublish.Load()) / 1e9, float64(s.lastRepair.Load()) / 1e9,
	}
	for index, value := range values {
		if err != nil && index < 16 {
			value = math.NaN()
		}
		kind := prometheus.GaugeValue
		if (index >= 4 && index <= 6) || index == 9 || (index >= 14 && index <= 20) {
			kind = prometheus.CounterValue
		}
		channel <- prometheus.MustNewConstMetric(buildDescriptors[index], kind, value)
	}
}
