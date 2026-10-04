// Package internalevent 可靠运输状态变化通知；业务事实始终由消费者回读 PostgreSQL。
package internalevent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type Ref struct {
	EventID         uuid.UUID `db:"event_id" json:"event_id"`
	Topic           string    `db:"topic" json:"topic"`
	AggregateID     uuid.UUID `db:"aggregate_id" json:"aggregate_id"`
	ProtocolVersion int       `db:"protocol_version" json:"protocol_version"`
}

type Reservation struct {
	Ref
	AvailableAt      time.Time `db:"available_at"`
	PublishToken     uuid.UUID `db:"publish_token"`
	ReservationCount int       `db:"reservation_count"`
}

type Module struct{ db *sqlx.DB }

type MetricsSnapshot struct {
	Pending      int     `db:"pending"`
	Published    int     `db:"published"`
	Quarantined  int     `db:"quarantined"`
	OldestAge    float64 `db:"oldest_age"`
	Reservations int64   `db:"reservations"`
}

func New(db *sqlx.DB) *Module { return &Module{db: db} }

// Reserve 取得短期运输权；它不代表事件已经被消费者处理。
func (m *Module) Reserve(ctx context.Context, limit int, lease time.Duration) ([]Reservation, error) {
	return m.reserve(ctx, limit, lease, nil)
}

// ReserveTopics 只领取指定业务消费者的事件，避免不同队列互相确认对方的业务事实。
func (m *Module) ReserveTopics(ctx context.Context, limit int, lease time.Duration, topics []string) ([]Reservation, error) {
	if len(topics) == 0 {
		return nil, errors.New("internal event topics are required")
	}
	return m.reserve(ctx, limit, lease, topics)
}

func (m *Module) reserve(ctx context.Context, limit int, lease time.Duration, topics []string) ([]Reservation, error) {
	if limit < 1 || limit > 1000 || lease <= 0 {
		return nil, errors.New("invalid internal event reservation")
	}
	now := time.Now().UTC()
	if _, err := m.db.ExecContext(ctx, `UPDATE internal_event_outbox SET state='quarantined',
		publish_token=NULL, publish_expires_at=NULL, last_error_code='unsupported_protocol_version', updated_at=$1
		WHERE id IN (SELECT id FROM internal_event_outbox WHERE state IN ('pending','published')
		AND protocol_version<>1 AND ($3::text[] IS NULL OR topic=ANY($3::text[]))
		ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2)`, now, limit, topics); err != nil {
		return nil, fmt.Errorf("quarantine internal events: %w", err)
	}
	items := make([]Reservation, 0)
	token := uuid.New()
	if err := m.db.SelectContext(ctx, &items, `WITH due AS (
		SELECT id FROM internal_event_outbox WHERE state IN ('pending','published')
		AND protocol_version=1 AND next_dispatch_at<=$1
		AND ($5::text[] IS NULL OR topic=ANY($5::text[]))
		AND (publish_expires_at IS NULL OR publish_expires_at<=$1)
		ORDER BY next_dispatch_at,id FOR UPDATE SKIP LOCKED LIMIT $2)
		UPDATE internal_event_outbox e SET publish_token=$3,publish_expires_at=$4,
		reservation_count=reservation_count+1,updated_at=$1 FROM due WHERE e.id=due.id
		RETURNING e.id AS event_id,e.topic,e.aggregate_id,e.protocol_version,e.available_at,
		e.publish_token,e.reservation_count`, now, limit, token, now.Add(lease), topics); err != nil {
		return nil, fmt.Errorf("reserve internal events: %w", err)
	}
	return items, nil
}

// ConfirmPublish 只确认当前 Token；迟到确认不能覆盖下一位投递者。
func (m *Module) ConfirmPublish(ctx context.Context, reservation Reservation, errorCode string, grace time.Duration) error {
	if reservation.PublishToken == uuid.Nil || grace <= 0 {
		return errors.New("invalid internal event confirmation")
	}
	now := time.Now().UTC()
	state, next := "published", now.Add(grace)
	var publishedAt *time.Time = &now
	if errorCode != "" {
		state, next, publishedAt = "pending", now.Add(grace), nil
	}
	_, err := m.db.ExecContext(ctx, `UPDATE internal_event_outbox SET state=$3,
		published_at=COALESCE(published_at,$4),next_dispatch_at=$5,publish_token=NULL,
		publish_expires_at=NULL,last_error_code=$6,updated_at=$7
		WHERE id=$1 AND publish_token=$2 AND state IN ('pending','published')`, reservation.EventID, reservation.PublishToken, state, publishedAt, next, nullString(errorCode), now)
	if err != nil {
		return fmt.Errorf("confirm internal event publish: %w", err)
	}
	return nil
}

// Resolve 校验完整引用后标记消费；重复或过期物理消息都幂等结束。
func (m *Module) Resolve(ctx context.Context, ref Ref) (bool, error) {
	if ref.EventID == uuid.Nil || ref.AggregateID == uuid.Nil || ref.ProtocolVersion != 1 || ref.Topic == "" {
		return false, errors.New("invalid internal event reference")
	}
	now := time.Now().UTC()
	result, err := m.db.ExecContext(ctx, `UPDATE internal_event_outbox SET state='consumed',
		consumed_at=COALESCE(consumed_at,$5),publish_token=NULL,publish_expires_at=NULL,updated_at=$5
		WHERE id=$1 AND topic=$2 AND aggregate_id=$3 AND protocol_version=$4
		AND state IN ('pending','published')`, ref.EventID, ref.Topic, ref.AggregateID, ref.ProtocolVersion, now)
	if err != nil {
		return false, fmt.Errorf("resolve internal event: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

type TraceMetadata struct {
	TraceParent string `db:"traceparent"`
	TraceState  string `db:"tracestate"`
}

// ReadForConsumption 同时校验完整引用和可消费状态，只返回持久化 Trace 元数据，不加载业务载荷。
func (m *Module) ReadForConsumption(ctx context.Context, ref Ref) (TraceMetadata, bool, error) {
	if ref.EventID == uuid.Nil || ref.AggregateID == uuid.Nil || ref.Topic == "" || ref.ProtocolVersion != 1 {
		return TraceMetadata{}, false, nil
	}
	var metadata TraceMetadata
	err := m.db.GetContext(ctx, &metadata, `SELECT traceparent,tracestate FROM internal_event_outbox
		WHERE id=$1 AND topic=$2 AND aggregate_id=$3 AND protocol_version=$4
		AND state IN ('pending','published')`, ref.EventID, ref.Topic, ref.AggregateID, ref.ProtocolVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return TraceMetadata{}, false, nil
	}
	return metadata, err == nil, err
}

// ReadMetricsSnapshot 从持久化事实重建事件积压，避免进程重启让核心指标归零。
func (m *Module) ReadMetricsSnapshot(ctx context.Context) (MetricsSnapshot, error) {
	var snapshot MetricsSnapshot
	err := m.db.GetContext(ctx, &snapshot, `SELECT
		count(*) FILTER (WHERE state='pending')::integer AS pending,
		count(*) FILTER (WHERE state='published')::integer AS published,
		count(*) FILTER (WHERE state='quarantined')::integer AS quarantined,
		COALESCE(EXTRACT(EPOCH FROM (CURRENT_TIMESTAMP-min(created_at)
			FILTER (WHERE state IN ('pending','published')))),0)::double precision AS oldest_age,
		COALESCE(sum(reservation_count),0)::bigint AS reservations
		FROM internal_event_outbox`)
	if err != nil {
		return MetricsSnapshot{}, fmt.Errorf("read internal event metrics: %w", err)
	}
	return snapshot, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
