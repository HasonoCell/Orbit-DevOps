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

func New(db *sqlx.DB) *Module { return &Module{db: db} }

// Reserve 取得短期运输权；它不代表事件已经被消费者处理。
func (m *Module) Reserve(ctx context.Context, limit int, lease time.Duration) ([]Reservation, error) {
	if limit < 1 || limit > 1000 || lease <= 0 {
		return nil, errors.New("invalid internal event reservation")
	}
	now := time.Now().UTC()
	if _, err := m.db.ExecContext(ctx, `UPDATE internal_event_outbox SET state='quarantined',
		publish_token=NULL, publish_expires_at=NULL, last_error_code='unsupported_protocol_version', updated_at=$1
		WHERE id IN (SELECT id FROM internal_event_outbox WHERE state IN ('pending','published')
		AND protocol_version<>1 ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2)`, now, limit); err != nil {
		return nil, fmt.Errorf("quarantine internal events: %w", err)
	}
	items := make([]Reservation, 0)
	token := uuid.New()
	if err := m.db.SelectContext(ctx, &items, `WITH due AS (
		SELECT id FROM internal_event_outbox WHERE state IN ('pending','published')
		AND protocol_version=1 AND next_dispatch_at<=$1
		AND (publish_expires_at IS NULL OR publish_expires_at<=$1)
		ORDER BY next_dispatch_at,id FOR UPDATE SKIP LOCKED LIMIT $2)
		UPDATE internal_event_outbox e SET publish_token=$3,publish_expires_at=$4,
		reservation_count=reservation_count+1,updated_at=$1 FROM due WHERE e.id=due.id
		RETURNING e.id AS event_id,e.topic,e.aggregate_id,e.protocol_version,e.available_at,
		e.publish_token,e.reservation_count`, now, limit, token, now.Add(lease)); err != nil {
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

// ExistsForConsumption 拒绝 Redis 中伪造或已经收束的引用，不向运输层泄露业务数据。
func (m *Module) ExistsForConsumption(ctx context.Context, ref Ref) (bool, error) {
	var exists bool
	err := m.db.GetContext(ctx, &exists, `SELECT EXISTS (SELECT 1 FROM internal_event_outbox
		WHERE id=$1 AND topic=$2 AND aggregate_id=$3 AND protocol_version=$4
		AND state IN ('pending','published'))`, ref.EventID, ref.Topic, ref.AggregateID, ref.ProtocolVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return exists, err
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
