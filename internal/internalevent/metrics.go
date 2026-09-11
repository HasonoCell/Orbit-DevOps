package internalevent

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var eventDescriptors = []*prometheus.Desc{
	prometheus.NewDesc("orbit_devops_internal_event_pending", "当前等待入队的内部事件数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_published", "已经入队但尚未消费的内部事件数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_quarantined", "协议隔离的内部事件数量。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_oldest_age_seconds", "最老未消费内部事件的年龄。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_reservations_total", "数据库记录的内部事件运输领取次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_send_errors_total", "本进程向 Redis 入队失败的次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_received_total", "本进程收到的合法内部事件引用次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_ignored_total", "本进程忽略的重复或过期事件次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_invalid_total", "本进程拒绝的非法事件协议次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_processing_errors_total", "本进程处理内部事件失败的次数。", nil, nil),
	prometheus.NewDesc("orbit_devops_internal_event_publish_success_timestamp_seconds", "事件发布循环最近成功时间。", nil, nil),
}

func (s *Service) Describe(channel chan<- *prometheus.Desc) {
	for _, descriptor := range eventDescriptors {
		channel <- descriptor
	}
}

// Collect 同时暴露可重建的数据库事实和当前进程运输结论。
func (s *Service) Collect(channel chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := s.events.ReadMetricsSnapshot(ctx)
	values := []float64{
		float64(snapshot.Pending), float64(snapshot.Published), float64(snapshot.Quarantined),
		snapshot.OldestAge, float64(snapshot.Reservations), float64(s.sendErrors.Load()),
		float64(s.received.Load()), float64(s.ignored.Load()), float64(s.invalid.Load()),
		float64(s.processingErrors.Load()), float64(s.lastPublish.Load()) / 1e9,
	}
	for index, value := range values {
		if err != nil && index < 5 {
			value = math.NaN()
		}
		kind := prometheus.GaugeValue
		if index >= 4 && index <= 9 {
			kind = prometheus.CounterValue
		}
		channel <- prometheus.MustNewConstMetric(eventDescriptors[index], kind, value)
	}
}

// Ready 要求数据库、Redis 和发布循环都可用；单个业务 Run 阻塞不影响就绪。
func (s *Service) Ready(ctx context.Context) error {
	if !s.running.Load() || time.Since(time.Unix(0, s.lastPublish.Load())) > max(3*s.config.PollInterval, 10*time.Second) {
		return errors.New("internal_event_publish_loop_unavailable")
	}
	if _, err := s.events.ReadMetricsSnapshot(ctx); err != nil {
		return errors.New("internal_event_store_unavailable")
	}
	if err := s.connection.Ping(ctx).Err(); err != nil {
		return errors.New("internal_event_queue_unavailable")
	}
	return nil
}
