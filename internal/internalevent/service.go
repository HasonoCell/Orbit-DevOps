package internalevent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/taskqueue"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

const TaskType = "orbit-devops:internal-event:v1"

type Executor interface {
	HandleEvent(context.Context, Ref) error
}

type Config struct {
	RedisAddress, RedisUsername, RedisPassword                   string
	RedisDB                                                      int
	Queue                                                        string
	Topics                                                       []string
	Concurrency                                                  int
	PollInterval, ConsumptionGrace, TaskTimeout, ShutdownTimeout time.Duration
	Logger                                                       *slog.Logger
}

type Service struct {
	config           Config
	events           *Module
	executor         Executor
	transport        *taskqueue.Transport
	started          atomic.Bool
	running          atomic.Bool
	lastPublish      atomic.Int64
	sendErrors       atomic.Uint64
	received         atomic.Uint64
	ignored          atomic.Uint64
	invalid          atomic.Uint64
	processingErrors atomic.Uint64
}

func NewService(config Config, events *Module, executor Executor) (*Service, error) {
	if config.RedisAddress == "" || events == nil || executor == nil || config.RedisDB < 0 || config.Concurrency < 1 || config.Concurrency > 100 || config.PollInterval <= 0 || config.ConsumptionGrace <= 0 || config.TaskTimeout <= 0 || config.ShutdownTimeout <= 0 {
		return nil, errors.New("invalid internal event service configuration")
	}
	if len(config.Topics) == 0 {
		config.Topics = []string{"webhook_delivery.received.v1", "build_operation.changed.v1", "release_operation.changed.v1", "delivery_run.reconcile.v1"}
	}
	seenTopics := make(map[string]struct{}, len(config.Topics))
	for _, topic := range config.Topics {
		if strings.TrimSpace(topic) != topic || topic == "" {
			return nil, errors.New("invalid internal event topic")
		}
		if _, exists := seenTopics[topic]; exists {
			return nil, errors.New("duplicate internal event topic")
		}
		seenTopics[topic] = struct{}{}
	}
	if config.Queue == "" {
		config.Queue = "orbit-devops-pipeline"
	}
	if strings.TrimSpace(config.Queue) == "" {
		return nil, errors.New("invalid internal event queue")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	service := &Service{config: config, events: events, executor: executor}
	transport, err := taskqueue.New(taskqueue.Config{RedisAddress: config.RedisAddress,
		RedisUsername: config.RedisUsername, RedisPassword: config.RedisPassword, RedisDB: config.RedisDB,
		Queue: config.Queue, Concurrency: config.Concurrency, PollInterval: config.PollInterval,
		TaskTimeout: config.TaskTimeout, ShutdownTimeout: config.ShutdownTimeout, MaxRetry: 8,
		Logger:  config.Logger,
		OnPanic: func() { service.processingErrors.Add(1) }})
	if err != nil {
		return nil, err
	}
	service.transport = transport
	return service, nil
}

func (s *Service) Close() error { return s.transport.Close() }

// PublishOnce 在数据库事务外并发发送有界批次，Redis 失败只延迟事件而不丢失事实。
func (s *Service) PublishOnce(ctx context.Context) error {
	items, err := s.events.ReserveTopics(ctx, min(s.config.Concurrency, 100), 10*time.Second, s.config.Topics)
	if err != nil {
		return errors.New("internal_event_store_unavailable")
	}
	var group sync.WaitGroup
	errorsByItem := make(chan error, len(items))
	for _, item := range items {
		group.Add(1)
		go func() { defer group.Done(); errorsByItem <- s.publish(ctx, item) }()
	}
	group.Wait()
	close(errorsByItem)
	var result error
	for itemErr := range errorsByItem {
		result = errors.Join(result, itemErr)
	}
	if result == nil {
		s.lastPublish.Store(time.Now().UnixNano())
	}
	return result
}

func (s *Service) publish(ctx context.Context, item Reservation) error {
	payload, err := json.Marshal(item.Ref)
	if err != nil {
		return errors.New("internal_event_encoding_failed")
	}
	_, sendErr := s.transport.Send(ctx, asynq.NewTask(TaskType, payload), item.AvailableAt)
	code := ""
	if sendErr != nil {
		code = "queue_unavailable"
		s.sendErrors.Add(1)
	}
	if err := s.events.ConfirmPublish(ctx, item, code, s.config.ConsumptionGrace); err != nil {
		return errors.New("internal_event_confirmation_unavailable")
	}
	if code != "" {
		return errors.New(code)
	}
	return nil
}

// Run 承载事件 Publisher 与 Consumer；业务处理失败由 Asynq 重投，数据库事件保持未消费。
func (s *Service) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("internal event service already started")
	}
	defer s.Close()
	if err := s.transport.Start(asynq.HandlerFunc(s.handle)); err != nil {
		return err
	}
	s.running.Store(true)
	defer s.running.Store(false)
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		cycle, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.PublishOnce(cycle)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.config.Logger.WarnContext(ctx, "内部事件发布暂时不可用")
		}
		select {
		case <-ctx.Done():
			s.transport.Stop()
			if err := s.transport.Close(); err != nil {
				return err
			}
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) handle(ctx context.Context, task *asynq.Task) (result error) {
	var ref Ref
	decoder := json.NewDecoder(bytes.NewReader(task.Payload()))
	decoder.DisallowUnknownFields()
	if task.Type() != TaskType || len(task.Payload()) > 1024 || decoder.Decode(&ref) != nil || decoder.Decode(new(any)) != io.EOF || ref.EventID == uuid.Nil || ref.AggregateID == uuid.Nil || ref.ProtocolVersion != 1 || ref.Topic == "" {
		s.invalid.Add(1)
		return asynq.SkipRetry
	}
	accepted := false
	for _, topic := range s.config.Topics {
		if topic == ref.Topic {
			accepted = true
			break
		}
	}
	if !accepted {
		s.invalid.Add(1)
		return asynq.SkipRetry
	}
	s.received.Add(1)
	exists, err := s.events.ExistsForConsumption(ctx, ref)
	if err != nil {
		s.processingErrors.Add(1)
		s.config.Logger.WarnContext(ctx, "内部事件存储暂时不可用", "topic", ref.Topic)
		return errors.New("internal_event_store_unavailable")
	}
	if !exists {
		s.ignored.Add(1)
		return nil
	}
	if err := s.executor.HandleEvent(ctx, ref); err != nil {
		s.processingErrors.Add(1)
		s.config.Logger.WarnContext(ctx, "内部事件处理暂时不可用", "topic", ref.Topic)
		return errors.New("internal_event_processing_interrupted")
	}
	resolved, err := s.events.Resolve(ctx, ref)
	if err != nil {
		s.processingErrors.Add(1)
		s.config.Logger.WarnContext(ctx, "内部事件确认暂时不可用", "topic", ref.Topic)
		return errors.New("internal_event_resolution_unavailable")
	}
	if !resolved {
		s.ignored.Add(1)
	}
	return nil
}
