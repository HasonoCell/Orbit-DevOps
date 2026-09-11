// Package webhook 验证并最小化持久化外部 Git Provider 的不可信 HTTP 投递。
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	StatePending     = "pending"
	StateProcessed   = "processed"
	StateIgnored     = "ignored"
	StateQuarantined = "quarantined"
)

var (
	ErrEndpointNotFound = errors.New("webhook endpoint not found")
	ErrMissingHeaders   = errors.New("required github webhook headers are missing")
	ErrInvalidSignature = errors.New("github webhook signature is invalid")
	ErrBodyTooLarge     = errors.New("github webhook body is too large")
	ErrSecurityConflict = errors.New("github delivery ID was reused with a different payload")
)

type EndpointSecrets struct {
	Current  string
	Previous string
}

type Config struct {
	Endpoints    map[string]EndpointSecrets
	MaxBodyBytes int64
}

type Headers struct {
	DeliveryID string
	EventType  string
	Signature  string
}

type Delivery struct {
	ID                 uuid.UUID  `db:"id"`
	EndpointKey        string     `db:"endpoint_key"`
	ProviderDeliveryID string     `db:"provider_delivery_id"`
	EventType          string     `db:"event_type"`
	PayloadDigest      string     `db:"payload_digest"`
	PayloadSize        int        `db:"payload_size"`
	State              string     `db:"state"`
	RepositoryID       *int64     `db:"repository_id"`
	RepositoryOwnerID  *int64     `db:"repository_owner_id"`
	RepositoryFullName *string    `db:"repository_full_name"`
	GitRef             *string    `db:"git_ref"`
	BeforeCommit       *string    `db:"before_commit"`
	AfterCommit        *string    `db:"after_commit"`
	Forced             bool       `db:"forced"`
	Deleted            bool       `db:"deleted"`
	ReasonCode         *string    `db:"reason_code"`
	TraceParent        string     `db:"traceparent"`
	TraceState         string     `db:"tracestate"`
	ReceivedAt         time.Time  `db:"received_at"`
	ProcessedAt        *time.Time `db:"processed_at"`
}

type Result struct {
	Delivery Delivery
	Replay   bool
}

type Module struct {
	db         *sqlx.DB
	config     Config
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

func New(db *sqlx.DB, config Config, tracer trace.Tracer, propagator propagation.TextMapPropagator) *Module {
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = 1024 * 1024
	}
	if tracer == nil {
		tracer = otel.Tracer("github.com/HasonoCell/Orbit-DevOps/internal/webhook")
	}
	if propagator == nil {
		propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	}
	return &Module{db: db, config: config, tracer: tracer, propagator: propagator}
}

// Accept 先按原始字节验签，再建立平台自己的 Trace 并原子持久化 Delivery 与事件意图。
func (m *Module) Accept(ctx context.Context, endpointKey string, headers Headers, body io.Reader) (Result, error) {
	secrets, ok := m.config.Endpoints[endpointKey]
	if !ok || strings.TrimSpace(secrets.Current) == "" {
		return Result{}, ErrEndpointNotFound
	}
	if strings.TrimSpace(headers.DeliveryID) == "" || strings.TrimSpace(headers.EventType) == "" || strings.TrimSpace(headers.Signature) == "" {
		return Result{}, ErrMissingHeaders
	}
	raw, err := io.ReadAll(io.LimitReader(body, m.config.MaxBodyBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read github webhook body: %w", err)
	}
	if int64(len(raw)) > m.config.MaxBodyBytes {
		return Result{}, ErrBodyTooLarge
	}
	if !validSignature(raw, headers.Signature, secrets) {
		return Result{}, ErrInvalidSignature
	}

	acceptedContext, span := m.tracer.Start(ctx, "github webhook accepted", trace.WithNewRoot())
	defer span.End()
	carrier := propagation.MapCarrier{}
	m.propagator.Inject(acceptedContext, carrier)
	delivery := normalize(headers, raw)
	delivery.ID = uuid.New()
	delivery.EndpointKey = endpointKey
	delivery.TraceParent = carrier.Get("traceparent")
	delivery.TraceState = carrier.Get("tracestate")
	delivery.ReceivedAt = time.Now().UTC()
	if delivery.State != StatePending {
		delivery.ProcessedAt = &delivery.ReceivedAt
	}

	tx, err := m.db.BeginTxx(acceptedContext, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin github webhook acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	inserted, err := insertDelivery(acceptedContext, tx, delivery)
	if err != nil {
		return Result{}, err
	}
	if !inserted {
		existing, err := getDelivery(acceptedContext, tx, endpointKey, headers.DeliveryID)
		if err != nil {
			return Result{}, err
		}
		if existing.PayloadDigest != delivery.PayloadDigest {
			return Result{}, ErrSecurityConflict
		}
		return Result{Delivery: existing, Replay: true}, nil
	}
	if delivery.State == StatePending {
		if _, err := tx.ExecContext(acceptedContext, `INSERT INTO internal_event_outbox
			(id, topic, aggregate_id, protocol_version, state, available_at, next_dispatch_at,
			 traceparent, tracestate, created_at, updated_at)
			VALUES ($1,'webhook_delivery.received.v1',$2,1,'pending',$3,$3,$4,$5,$3,$3)`, uuid.New(), delivery.ID, delivery.ReceivedAt, delivery.TraceParent, delivery.TraceState); err != nil {
			return Result{}, fmt.Errorf("enqueue github webhook delivery: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit github webhook acceptance: %w", err)
	}
	return Result{Delivery: delivery}, nil
}

func validSignature(body []byte, supplied string, secrets EndpointSecrets) bool {
	supplied = strings.TrimSpace(supplied)
	if !strings.HasPrefix(supplied, "sha256=") {
		return false
	}
	supplied = strings.TrimPrefix(supplied, "sha256=")
	decoded, err := hex.DecodeString(supplied)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	matched := false
	// 两个 Secret 都执行 HMAC，避免通过响应时间判断是否命中轮换窗口。
	for _, secret := range []string{secrets.Current, secrets.Previous} {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		candidate := mac.Sum(nil)
		valid := secret != "" && hmac.Equal(decoded, candidate)
		matched = matched || valid
	}
	return matched
}

type pushPayload struct {
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Forced     bool   `json:"forced"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Owner    struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repository"`
}

func normalize(headers Headers, raw []byte) Delivery {
	hash := sha256.Sum256(raw)
	delivery := Delivery{ProviderDeliveryID: strings.TrimSpace(headers.DeliveryID), EventType: strings.TrimSpace(headers.EventType), PayloadDigest: "sha256:" + hex.EncodeToString(hash[:]), PayloadSize: len(raw)}
	if delivery.EventType != "push" {
		reason := "unsupported_event"
		delivery.State, delivery.ReasonCode = StateIgnored, &reason
		return delivery
	}
	var payload pushPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF || !validPush(payload) {
		reason := "invalid_push_payload"
		delivery.State, delivery.ReasonCode = StateQuarantined, &reason
		return delivery
	}
	delivery.RepositoryID = &payload.Repository.ID
	delivery.RepositoryOwnerID = &payload.Repository.Owner.ID
	delivery.RepositoryFullName = &payload.Repository.FullName
	delivery.GitRef = &payload.Ref
	delivery.BeforeCommit = &payload.Before
	delivery.AfterCommit = &payload.After
	delivery.Forced, delivery.Deleted = payload.Forced, payload.Deleted
	if payload.Deleted {
		reason := "branch_deleted"
		delivery.State, delivery.ReasonCode = StateIgnored, &reason
	} else {
		delivery.State = StatePending
	}
	return delivery
}

func validPush(payload pushPayload) bool {
	return payload.Repository.ID > 0 && payload.Repository.Owner.ID > 0 && strings.TrimSpace(payload.Repository.FullName) != "" && strings.HasPrefix(payload.Ref, "refs/heads/") && validCommit(payload.Before) && validCommit(payload.After)
}

func validCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func insertDelivery(ctx context.Context, tx *sqlx.Tx, delivery Delivery) (bool, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries
		(id, endpoint_key, provider_delivery_id, event_type, payload_digest, payload_size, state,
		 repository_id, repository_owner_id, repository_full_name, git_ref, before_commit, after_commit,
		 forced, deleted, reason_code, traceparent, tracestate, received_at, processed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (endpoint_key, provider_delivery_id) DO NOTHING`, delivery.ID, delivery.EndpointKey, delivery.ProviderDeliveryID, delivery.EventType, delivery.PayloadDigest, delivery.PayloadSize, delivery.State, delivery.RepositoryID, delivery.RepositoryOwnerID, delivery.RepositoryFullName, delivery.GitRef, delivery.BeforeCommit, delivery.AfterCommit, delivery.Forced, delivery.Deleted, delivery.ReasonCode, delivery.TraceParent, delivery.TraceState, delivery.ReceivedAt, delivery.ProcessedAt)
	if err != nil {
		return false, fmt.Errorf("insert github webhook delivery: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func getDelivery(ctx context.Context, queryer sqlx.QueryerContext, endpointKey, deliveryID string) (Delivery, error) {
	var delivery Delivery
	err := sqlx.GetContext(ctx, queryer, &delivery, `SELECT id, endpoint_key, provider_delivery_id, event_type,
		payload_digest, payload_size, state, repository_id, repository_owner_id, repository_full_name,
		git_ref, before_commit, after_commit, forced, deleted, reason_code, traceparent, tracestate,
		received_at, processed_at FROM webhook_deliveries WHERE endpoint_key=$1 AND provider_delivery_id=$2`, endpointKey, deliveryID)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, errors.New("webhook delivery disappeared during deduplication")
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("load github webhook delivery: %w", err)
	}
	return delivery, nil
}
