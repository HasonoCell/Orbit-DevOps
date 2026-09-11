package releasedispatch

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Ready 要求 PostgreSQL、Redis 与两个维护循环均可用；业务失败不影响进程就绪。
func (s *Service) Ready(ctx context.Context) error {
	if !s.running.Load() || time.Since(time.Unix(0, s.lastPublish.Load())) > max(3*s.config.PollInterval, 10*time.Second) ||
		time.Since(time.Unix(0, s.lastRepair.Load())) > max(3*s.config.RepairInterval, 10*time.Second) {
		return errors.New("dispatch_loops_unavailable")
	}
	if _, err := s.releaseOperations.ReadDispatchMetrics(ctx); err != nil {
		return errors.New("dispatch_store_unavailable")
	}
	if err := s.connection.Ping(ctx).Err(); err != nil {
		return errors.New("queue_unavailable")
	}
	return nil
}

// ReadinessHandler 不返回连接地址或驱动错误；原 healthz 保持独立兼容。
func (s *Service) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if s.Ready(ctx) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// 指标只有固定名称/类别，不使用 ReleaseOperation、Task 或地址作为标签。
var releaseDispatchDescriptors = []*prometheus.Desc{
	prometheus.NewDesc("orbit_devops_release_dispatch_pending", "当前有效的待投递意图数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_published", "已入队但尚未取得业务执行权的意图数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_quarantined", "需要受控修复的协议隔离意图数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_oldest_age_seconds", "最老有效待办年龄；数据库不可用时为 NaN。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_reservations", "数据库记录的运输领取累计次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_redeliveries", "数据库记录的同代次补发累计次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_send_errors_total", "本进程入队失败次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_received_total", "本进程收到的合法引用次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_ignored_total", "本进程忽略的过期或重复消息次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_invalid_total", "本进程丢弃的非法协议消息次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_execution_errors_total", "本进程消费基础设施错误次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_publish_success_timestamp_seconds", "投递循环最近成功时间。", nil, nil),
	prometheus.NewDesc("orbit_devops_release_dispatch_repair_success_timestamp_seconds", "补偿循环最近成功时间。", nil, nil),
}

func (s *Service) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range releaseDispatchDescriptors {
		ch <- desc
	}
}

// Collect 每次读取一致快照；依赖故障显式输出 NaN，保留循环最后成功时间供告警。
func (s *Service) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, err := s.releaseOperations.ReadDispatchMetrics(ctx)
	values := []float64{float64(state.Pending), float64(state.Published), float64(state.Quarantined), state.OldestAge,
		float64(state.Reservations), float64(state.Redeliveries), float64(s.sendErrors.Load()), float64(s.received.Load()), float64(s.ignored.Load()), float64(s.invalid.Load()), float64(s.executionErrors.Load()),
		float64(s.lastPublish.Load()) / 1e9, float64(s.lastRepair.Load()) / 1e9}
	for i, value := range values {
		if err != nil && i < 6 {
			value = math.NaN()
		}
		kind := prometheus.GaugeValue
		if i >= 6 && i <= 10 {
			kind = prometheus.CounterValue
		}
		ch <- prometheus.MustNewConstMetric(releaseDispatchDescriptors[i], kind, value)
	}
}
