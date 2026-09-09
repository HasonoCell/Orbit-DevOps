// Package builddispatch 将持久化 BuildDispatch 接入 Asynq；业务执行权仍由 BuildOperation 拥有。
package builddispatch

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

	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	redisclient "github.com/redis/go-redis/v9"
)

const TaskType = "orbitops:build-dispatch:v1"

// Executor 负责回 PostgreSQL 取得业务执行权；运输 Service 不判断构建状态。
type Executor interface {
	RunDispatch(context.Context, buildoperation.DispatchRef) (buildoperation.ClaimOutcome, error)
}

type Config struct {
	RedisAddress     string
	RedisUsername    string
	RedisPassword    string
	RedisDB          int
	Queue            string
	Concurrency      int
	PollInterval     time.Duration
	RepairInterval   time.Duration
	ConsumptionGrace time.Duration
	TaskTimeout      time.Duration
	ShutdownTimeout  time.Duration
	Logger           *slog.Logger
	AfterEnqueue     func(buildoperation.Dispatch)
}

type Service struct {
	config          Config
	operations      *buildoperation.Module
	executor        Executor
	connection      *redisclient.Client
	client          *asynq.Client
	redis           asynq.RedisClientOpt
	started         atomic.Bool
	running         atomic.Bool
	lastPublish     atomic.Int64
	lastRepair      atomic.Int64
	sendErrors      atomic.Uint64
	received        atomic.Uint64
	ignored         atomic.Uint64
	invalid         atomic.Uint64
	executionErrors atomic.Uint64
	workersMu       sync.Mutex
	workers         sync.WaitGroup
	closing         bool
}

func New(config Config, operations *buildoperation.Module, executor Executor) (*Service, error) {
	if config.RedisAddress == "" || operations == nil || executor == nil || config.RedisDB < 0 ||
		config.Concurrency < 1 || config.Concurrency > 100 || config.PollInterval <= 0 ||
		config.RepairInterval <= 0 || config.ConsumptionGrace <= 0 || config.TaskTimeout <= 0 || config.ShutdownTimeout <= 0 {
		return nil, errors.New("invalid build dispatch configuration")
	}
	if config.Queue == "" {
		config.Queue = "orbitops-build"
	}
	if strings.TrimSpace(config.Queue) == "" {
		return nil, errors.New("invalid build dispatch queue name")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	redis := asynq.RedisClientOpt{
		Addr: config.RedisAddress, Username: config.RedisUsername, Password: config.RedisPassword,
		DB: config.RedisDB, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
	}
	connection := redisclient.NewClient(&redisclient.Options{
		Addr: config.RedisAddress, Username: config.RedisUsername, Password: config.RedisPassword,
		DB: config.RedisDB, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		ContextTimeoutEnabled: true, MaxRetries: -1,
	})
	return &Service{config: config, operations: operations, executor: executor, redis: redis,
		connection: connection, client: asynq.NewClientFromRedisClient(connection)}, nil
}

func (s *Service) Close() error { return s.connection.Close() }

// PublishOnce 在数据库事务外发送消息；确认失败时保留同一持久化意图供补发。
func (s *Service) PublishOnce(ctx context.Context) error {
	items, err := s.operations.ReserveDispatches(ctx, min(s.config.Concurrency, 100), 10*time.Second)
	if err != nil {
		return errors.New("build_dispatch_store_unavailable")
	}
	var sends sync.WaitGroup
	errorsByItem := make(chan error, len(items))
	for _, item := range items {
		sends.Add(1)
		go func() {
			defer sends.Done()
			errorsByItem <- s.publish(ctx, item)
		}()
	}
	sends.Wait()
	close(errorsByItem)
	var result error
	for err := range errorsByItem {
		result = errors.Join(result, err)
	}
	if result == nil {
		s.lastPublish.Store(time.Now().UnixNano())
	}
	return result
}

func (s *Service) publish(ctx context.Context, item buildoperation.Dispatch) error {
	payload, err := json.Marshal(item.DispatchRef)
	if err != nil {
		return errors.New("build_dispatch_encoding_failed")
	}
	sendContext, cancel := context.WithTimeout(ctx, time.Second)
	_, sendErr := s.client.EnqueueContext(sendContext, asynq.NewTask(TaskType, payload),
		asynq.Queue(s.config.Queue), asynq.ProcessAt(item.AvailableAt), asynq.MaxRetry(5), asynq.Timeout(s.config.TaskTimeout))
	cancel()
	code := ""
	if sendErr != nil {
		code = "queue_unavailable"
		s.sendErrors.Add(1)
	} else if s.config.AfterEnqueue != nil {
		s.config.AfterEnqueue(item)
	}
	if err := s.operations.ConfirmDispatch(ctx, item, code, s.config.ConsumptionGrace); err != nil {
		return errors.New("build_dispatch_confirmation_unavailable")
	}
	if code != "" {
		return errors.New(code)
	}
	return nil
}

// Run 同时运行投递、消费和过期 Lease 修复；进程退出不等于用户取消 Build。
func (s *Service) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("build dispatch service already started")
	}
	defer s.Close()
	workerContext, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	server := asynq.NewServer(s.redis, asynq.Config{
		Concurrency: s.config.Concurrency, Queues: map[string]int{s.config.Queue: 1},
		BaseContext: func() context.Context { return workerContext }, TaskCheckInterval: s.config.PollInterval,
		DelayedTaskCheckInterval: s.config.PollInterval, ShutdownTimeout: s.config.ShutdownTimeout,
		Logger: quietLogger{s.config.Logger}, LogLevel: asynq.ErrorLevel,
		RetryDelayFunc: func(n int, _ error, _ *asynq.Task) time.Duration {
			return min(time.Second*time.Duration(1<<min(n, 5)), 30*time.Second)
		},
	})
	if err := server.Start(asynq.HandlerFunc(s.handle)); err != nil {
		return errors.New("build_queue_start_failed")
	}
	s.running.Store(true)
	var loops sync.WaitGroup
	loops.Add(2)
	go func() { defer loops.Done(); s.loop(ctx, s.config.PollInterval, s.PublishOnce) }()
	go func() {
		defer loops.Done()
		s.loop(ctx, s.config.RepairInterval, func(cycle context.Context) error {
			_, err := s.operations.RepairDispatches(cycle, 100)
			if err == nil {
				s.lastRepair.Store(time.Now().UnixNano())
			}
			return err
		})
	}()
	<-ctx.Done()
	s.running.Store(false)
	server.Stop()
	loops.Wait()
	server.Shutdown()
	s.workersMu.Lock()
	s.closing = true
	cancelWorkers()
	s.workersMu.Unlock()
	s.workers.Wait()
	return nil
}

func (s *Service) loop(ctx context.Context, interval time.Duration, run func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		cycle, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := run(cycle)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.config.Logger.WarnContext(ctx, "构建分发维护暂时不可用")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) handle(ctx context.Context, task *asynq.Task) (result error) {
	s.workersMu.Lock()
	if s.closing {
		s.workersMu.Unlock()
		return errors.New("build_worker_stopping")
	}
	s.workers.Add(1)
	s.workersMu.Unlock()
	defer s.workers.Done()
	defer func() {
		if recover() != nil {
			s.executionErrors.Add(1)
			result = errors.New("build_dispatch_handler_interrupted")
		}
	}()
	var ref buildoperation.DispatchRef
	decoder := json.NewDecoder(bytes.NewReader(task.Payload()))
	decoder.DisallowUnknownFields()
	if task.Type() != TaskType || len(task.Payload()) > 1024 || decoder.Decode(&ref) != nil || decoder.Decode(new(any)) != io.EOF ||
		ref.ProtocolVersion != 1 || ref.Sequence <= 0 || ref.BuildOperationID == uuid.Nil || ref.DispatchID == uuid.Nil {
		s.invalid.Add(1)
		return asynq.SkipRetry
	}
	s.received.Add(1)
	outcome, err := s.executor.RunDispatch(ctx, ref)
	if err != nil {
		s.executionErrors.Add(1)
		return errors.New("build_dispatch_execution_interrupted")
	}
	if outcome == buildoperation.ClaimOutcomeIgnored {
		s.ignored.Add(1)
	}
	return nil
}

type quietLogger struct{ logger *slog.Logger }

func (l quietLogger) Debug(...interface{}) {}
func (l quietLogger) Info(...interface{})  {}
func (l quietLogger) Warn(...interface{})  { l.logger.Warn("Asynq 构建队列警告") }
func (l quietLogger) Error(...interface{}) { l.logger.Error("Asynq 构建队列暂时不可用") }
func (l quietLogger) Fatal(...interface{}) { l.logger.Error("Asynq 构建队列运行失败") }
