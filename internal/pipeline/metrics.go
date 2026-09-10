package pipeline

import (
	"context"
	"math"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
)

type labelCount struct {
	Label string `db:"label"`
	Count int    `db:"count"`
}

type Metrics struct {
	db              *sqlx.DB
	githubReads     *prometheus.CounterVec
	transitions     *prometheus.CounterVec
	stageDuration   *prometheus.HistogramVec
	maintenanceRuns *prometheus.CounterVec
	runStatus       *prometheus.Desc
	blockedReasons  *prometheus.Desc
	webhookStates   *prometheus.Desc
}

// NewMetrics 将进程内结论与可从 PostgreSQL 重建的当前状态合并为一个 Collector。
func NewMetrics(db *sqlx.DB) *Metrics {
	return &Metrics{
		db: db,
		githubReads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbitops", Name: "pipeline_github_reads_total", Help: "Pipeline Worker 按结论统计的 GitHub 权威回读次数。",
		}, []string{"outcome"}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbitops", Name: "delivery_run_transitions_total", Help: "DeliveryRun 按阶段、结果与稳定原因统计的推进次数。",
		}, []string{"stage", "outcome", "reason"}),
		stageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orbitops", Name: "delivery_run_stage_duration_seconds", Help: "DeliveryRun 已完成阶段的耗时。", Buckets: prometheus.ExponentialBuckets(0.1, 2, 16),
		}, []string{"stage", "outcome"}),
		maintenanceRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orbitops", Name: "pipeline_maintenance_total", Help: "Pipeline 修复和保留期维护的运行结论。",
		}, []string{"outcome"}),
		runStatus:      prometheus.NewDesc("orbitops_delivery_runs", "按对外投影状态统计的 DeliveryRun 当前数量。", []string{"status"}, nil),
		blockedReasons: prometheus.NewDesc("orbitops_delivery_run_blocked_reasons", "按稳定原因统计的已阻塞 DeliveryRun 数量。", []string{"reason"}, nil),
		webhookStates:  prometheus.NewDesc("orbitops_webhook_deliveries", "按状态统计的 WebhookDelivery 数量。", []string{"state"}, nil),
	}
}

func (m *Metrics) Describe(channel chan<- *prometheus.Desc) {
	m.githubReads.Describe(channel)
	m.transitions.Describe(channel)
	m.stageDuration.Describe(channel)
	m.maintenanceRuns.Describe(channel)
	channel <- m.runStatus
	channel <- m.blockedReasons
	channel <- m.webhookStates
}

func (m *Metrics) Collect(channel chan<- prometheus.Metric) {
	m.githubReads.Collect(channel)
	m.transitions.Collect(channel)
	m.stageDuration.Collect(channel)
	m.maintenanceRuns.Collect(channel)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.collectCounts(ctx, channel, m.runStatus, runStatusMetricsQuery)
	m.collectCounts(ctx, channel, m.blockedReasons, `SELECT COALESCE(reason_code,'unknown') AS label,count(*)::integer AS count FROM delivery_runs WHERE phase='blocked' GROUP BY reason_code`)
	m.collectCounts(ctx, channel, m.webhookStates, `SELECT state AS label,count(*)::integer AS count FROM webhook_deliveries GROUP BY state`)
}

func (m *Metrics) collectCounts(ctx context.Context, channel chan<- prometheus.Metric, descriptor *prometheus.Desc, query string) {
	rows := make([]labelCount, 0)
	if m.db == nil || m.db.SelectContext(ctx, &rows, query) != nil {
		channel <- prometheus.MustNewConstMetric(descriptor, prometheus.GaugeValue, math.NaN(), "unavailable")
		return
	}
	for _, row := range rows {
		channel <- prometheus.MustNewConstMetric(descriptor, prometheus.GaugeValue, float64(row.Count), row.Label)
	}
}

func (m *Metrics) RecordGitHubRead(outcome string) {
	m.githubReads.WithLabelValues(outcome).Inc()
}

func (m *Metrics) RecordTransition(stage, outcome, reason string, duration time.Duration) {
	if reason == "" {
		reason = "none"
	}
	m.transitions.WithLabelValues(stage, outcome, reason).Inc()
	if duration > 0 {
		m.stageDuration.WithLabelValues(stage, outcome).Observe(duration.Seconds())
	}
}

func (m *Metrics) RecordMaintenance(outcome string) {
	m.maintenanceRuns.WithLabelValues(outcome).Inc()
}

const runStatusMetricsQuery = `SELECT status AS label,count(*)::integer AS count FROM (
	SELECT CASE
		WHEN dr.phase='build_created' AND bo.status='failed' THEN 'build_failed'
		WHEN dr.phase='build_created' AND bo.status='canceled' THEN 'build_canceled'
		WHEN dr.phase='build_created' AND bo.status='attention_required' THEN 'attention_required'
		WHEN dr.phase='build_created' THEN 'building'
		WHEN dr.phase='artifact_ready' AND pr.mode='auto_release' AND dr.source_check_attempt_count>0 THEN 'verifying_source'
		WHEN dr.phase='artifact_ready' THEN 'candidate_ready'
		WHEN dr.phase='release_created' AND ro.status='failed' THEN 'release_failed'
		WHEN dr.phase='release_created' AND ro.status='canceled' THEN 'release_canceled'
		WHEN dr.phase='release_created' AND ro.status='attention_required' THEN 'attention_required'
		WHEN dr.phase='release_created' THEN 'releasing'
		WHEN dr.phase='completed' THEN 'succeeded'
		ELSE dr.phase END AS status
	FROM delivery_runs dr
	JOIN delivery_pipeline_revisions pr ON pr.delivery_pipeline_id=dr.delivery_pipeline_id AND pr.revision=dr.pipeline_revision
	JOIN build_operations bo ON bo.build_id=dr.build_id
	LEFT JOIN release_operations ro ON ro.release_id=dr.release_id
) projected GROUP BY status`
