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

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	redisclient "github.com/redis/go-redis/v9"
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
	connection       *redisclient.Client
	client           *asynq.Client
	redis            asynq.RedisClientOpt
	started          atomic.Bool
	running          atomic.Bool
	lastPublish      atomic.Int64
	sendErrors       atomic.Uint64
	received         atomic.Uint64
	ignored          atomic.Uint64
	invalid          atomic.Uint64
	processingErrors atomic.Uint64
	workers          sync.WaitGroup
	mu               sync.Mutex
	closing          bool
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
	redis := asynq.RedisClientOpt{Addr: config.RedisAddress, Username: config.RedisUsername, Password: config.RedisPassword, DB: config.RedisDB, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second}
	connection := redisclient.NewClient(&redisclient.Options{Addr: config.RedisAddress, Username: config.RedisUsername, Password: config.RedisPassword, DB: config.RedisDB, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true, MaxRetries: -1})
	return &Service{config: config, events: events, executor: executor, redis: redis, connection: connection, client: asynq.NewClientFromRedisClient(connection)}, nil
}

func (s *Service) Close() error { return s.connection.Close() }

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
	sendContext, cancel := context.WithTimeout(ctx, time.Second)
	_, sendErr := s.client.EnqueueContext(sendContext, asynq.NewTask(TaskType, payload), asynq.Queue(s.config.Queue), asynq.ProcessAt(item.AvailableAt), asynq.MaxRetry(8), asynq.Timeout(s.config.TaskTimeout))
	cancel()
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
	workerContext, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	server := asynq.NewServer(s.redis, asynq.Config{Concurrency: s.config.Concurrency, Queues: map[string]int{s.config.Queue: 1}, BaseContext: func() context.Context { return workerContext }, TaskCheckInterval: s.config.PollInterval, DelayedTaskCheckInterval: s.config.PollInterval, ShutdownTimeout: s.config.ShutdownTimeout, Logger: quietLogger{s.config.Logger}, LogLevel: asynq.ErrorLevel})
	if err := server.Start(asynq.HandlerFunc(s.handle)); err != nil {
		return errors.New("internal_event_queue_start_failed")
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
			server.Stop()
			server.Shutdown()
			s.mu.Lock()
			s.closing = true
			cancelWorkers()
			s.mu.Unlock()
			s.workers.Wait()
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) handle(ctx context.Context, task *asynq.Task) (result error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return errors.New("pipeline_worker_stopping")
	}
	s.workers.Add(1)
	s.mu.Unlock()
	defer s.workers.Done()
	defer func() {
		if recover() != nil {
			s.processingErrors.Add(1)
			s.config.Logger.WarnContext(ctx, "内部事件处理被中断")
			result = errors.New("internal_event_handler_interrupted")
		}
	}()
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

type quietLogger struct{ logger *slog.Logger }

func (quietLogger) Debug(args ...interface{}) {}
func (quietLogger) Info(args ...interface{})  {}
func (quietLogger) Warn(args ...interface{})  { quietLogger{}.discard(args...) }
func (quietLogger) Error(args ...interface{}) { quietLogger{}.discard(args...) }
func (quietLogger) Fatal(args ...interface{}) { quietLogger{}.discard(args...) }
func (quietLogger) discard(...interface{})    {}
