// Package dispatch 将持久化执行意图接入 Asynq；不拥有发布状态机或 Kubernetes 权限。
package dispatch

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

	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	redisclient "github.com/redis/go-redis/v9"
)

const TaskType = "orbitops:release-dispatch:v1"

// Executor 的实现负责数据库执行权与业务结果，返回值只描述本条意图是否已处理。
type Executor interface {
	RunDispatch(context.Context, operation.DispatchRef) (operation.ClaimOutcome, error)
}

// Config 将运输预算与业务预算分离；密码只用于建立连接，不进入任务或日志。
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
	// AfterEnqueue 暴露真实运输成功但数据库确认前的进程边界，供中断验收使用。
	AfterEnqueue func(operation.Dispatch)
}

// Service 在同一 Worker 中管理投递、消费、补偿与退出；每个实例只能运行一次。
type Service struct {
	config          Config
	operations      *operation.Module
	executor        Executor
	client          *asynq.Client
	connection      *redisclient.Client
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
	closing         bool
	workers         sync.WaitGroup
}

// New 校验运行参数；创建对象不会领取或执行任务。
func New(config Config, operations *operation.Module, executor Executor) (*Service, error) {
	if config.RedisAddress == "" || operations == nil || executor == nil || config.Concurrency < 1 || config.Concurrency > 100 ||
		config.PollInterval <= 0 || config.RepairInterval <= 0 || config.ConsumptionGrace <= 0 || config.TaskTimeout <= 0 || config.ShutdownTimeout <= 0 || config.RedisDB < 0 {
		return nil, errors.New("invalid dispatch configuration")
	}
	if config.Queue == "" {
		config.Queue = "orbitops-release"
	}
	if strings.TrimSpace(config.Queue) == "" {
		return nil, errors.New("invalid dispatch queue name")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// go-redis 的底层日志是进程级全局入口，不能让它绕过安全日志直接打印原始连接错误。
	redisLogOnce.Do(func() { redisclient.SetLogger(discardRedisLogger{}) })
	redis := asynq.RedisClientOpt{Addr: config.RedisAddress, Username: config.RedisUsername, Password: config.RedisPassword,
		DB: config.RedisDB, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second}
	connection := redisclient.NewClient(&redisclient.Options{Addr: config.RedisAddress, Username: config.RedisUsername, Password: config.RedisPassword,
		DB: config.RedisDB, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true, MaxRetries: -1})
	return &Service{config: config, operations: operations, executor: executor, redis: redis, connection: connection, client: asynq.NewClientFromRedisClient(connection)}, nil
}

// Close 释放投递连接；Run 自动调用，单独执行 PublishOnce 的调用方需在结束后调用。
func (s *Service) Close() error { return s.connection.Close() }

// PublishOnce 先取得短期投递 token，再在事务外入队；确认失败不会抹去原意图。
func (s *Service) PublishOnce(ctx context.Context) error {
	// 每条外部请求最多一秒；批次按并发上限控制，避免 token 在排队发送期间失效。
	items, err := s.operations.ReserveDispatches(ctx, min(s.config.Concurrency, 100), 10*time.Second)
	if err != nil {
		return errors.New("dispatch_store_unavailable")
	}
	var sends sync.WaitGroup
	errorsByItem := make(chan error, len(items))
	for _, item := range items {
		sends.Add(1)
		go func() { defer sends.Done(); errorsByItem <- s.publish(ctx, item) }()
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

// publish 每条发送有独立截止时间；批次有界并发，不让排队网络等待耗尽投递租约。
func (s *Service) publish(ctx context.Context, item operation.Dispatch) error {
	if item.ReservationCount >= 5 {
		s.config.Logger.WarnContext(ctx, "发布意图持续未消费，正在退避补发", "operation_id", item.OperationID, "dispatch_id", item.DispatchID, "sequence", item.Sequence, "reservation_count", item.ReservationCount)
	}
	payload, err := json.Marshal(item.DispatchRef)
	if err != nil {
		return errors.New("dispatch_encoding_failed")
	}
	sendContext, cancel := context.WithTimeout(ctx, time.Second)
	info, sendErr := s.client.EnqueueContext(sendContext, asynq.NewTask(TaskType, payload),
		asynq.Queue(s.config.Queue), asynq.ProcessAt(item.AvailableAt), asynq.MaxRetry(5), asynq.Timeout(s.config.TaskTimeout))
	cancel()
	code := ""
	if sendErr != nil {
		code = "queue_unavailable"
		s.sendErrors.Add(1)
	} else {
		s.config.Logger.InfoContext(ctx, "发布意图已入队", "operation_id", item.OperationID, "dispatch_id", item.DispatchID, "sequence", item.Sequence, "task_id", info.ID)
		if s.config.AfterEnqueue != nil {
			s.config.AfterEnqueue(item)
		}
	}
	if err := s.operations.ConfirmDispatch(ctx, item, code, s.config.ConsumptionGrace); err != nil {
		return errors.New("dispatch_confirmation_unavailable")
	}
	if code != "" {
		return errors.New(code)
	}
	return nil
}

// Run 启动真实消费者及两个有界维护循环。进程退出不等于用户取消发布。
func (s *Service) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("dispatch service already started")
	}
	defer s.Close()
	workerContext, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	server := asynq.NewServer(s.redis, asynq.Config{
		Concurrency: s.config.Concurrency, Queues: map[string]int{s.config.Queue: 1},
		BaseContext:       func() context.Context { return workerContext },
		TaskCheckInterval: s.config.PollInterval, DelayedTaskCheckInterval: s.config.PollInterval,
		ShutdownTimeout: s.config.ShutdownTimeout, Logger: safeLogger{s.config.Logger}, LogLevel: asynq.ErrorLevel,
		RetryDelayFunc: func(n int, _ error, _ *asynq.Task) time.Duration {
			return min(time.Second*time.Duration(1<<min(n, 5)), 30*time.Second)
		},
	})
	if err := server.Start(asynq.HandlerFunc(s.handle)); err != nil {
		return errors.New("queue_start_failed")
	}
	s.running.Store(true)
	var loops sync.WaitGroup
	loops.Add(2)
	go func() { defer loops.Done(); s.loop(ctx, s.config.PollInterval, s.PublishOnce) }()
	go func() {
		defer loops.Done()
		s.loop(ctx, s.config.RepairInterval, func(ctx context.Context) error {
			_, err := s.operations.RepairDispatches(ctx, 100)
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
	// Asynq 退出可能早于 Handler 返回，必须等业务调用响应取消后才能关闭数据库。
	s.workers.Wait()
	return nil
}

// loop 为每轮数据库维护设置超时；错误使用固定分类，避免连接材料进入日志。
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
			s.config.Logger.WarnContext(ctx, "分发维护暂时不可用")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// handle 只接受受限引用。损坏消息不改合法意图；原始业务错误不进入 Redis 归档。
func (s *Service) handle(ctx context.Context, task *asynq.Task) (result error) {
	s.workersMu.Lock()
	if s.closing {
		s.workersMu.Unlock()
		return errors.New("worker_stopping")
	}
	s.workers.Add(1)
	s.workersMu.Unlock()
	defer s.workers.Done()
	// 在框架之前收束 panic，防止框架将任意 panic 内容写入日志或 Redis。
	defer func() {
		if recover() != nil {
			s.executionErrors.Add(1)
			result = errors.New("dispatch_handler_interrupted")
		}
	}()
	var ref operation.DispatchRef
	decoder := json.NewDecoder(bytes.NewReader(task.Payload()))
	decoder.DisallowUnknownFields()
	if task.Type() != TaskType || len(task.Payload()) > 1024 || decoder.Decode(&ref) != nil || decoder.Decode(new(any)) != io.EOF ||
		ref.ProtocolVersion != 1 || ref.Sequence <= 0 || ref.OperationID == uuid.Nil || ref.DispatchID == uuid.Nil {
		s.invalid.Add(1)
		s.config.Logger.WarnContext(ctx, "忽略非法投递消息")
		return asynq.SkipRetry
	}
	s.received.Add(1)
	taskID, _ := asynq.GetTaskID(ctx)
	// 物理消息元数据也可能被外部构造；只记录本系统生成的 UUID 形态标识。
	if _, err := uuid.Parse(taskID); err != nil {
		taskID = "invalid"
	}
	s.config.Logger.InfoContext(ctx, "消费发布意图", "operation_id", ref.OperationID, "dispatch_id", ref.DispatchID, "sequence", ref.Sequence, "task_id", taskID)
	outcome, err := s.executor.RunDispatch(ctx, ref)
	if err != nil {
		s.executionErrors.Add(1)
		return errors.New("dispatch_execution_interrupted")
	}
	if outcome == operation.ClaimOutcomeIgnored {
		s.ignored.Add(1)
	}
	s.config.Logger.InfoContext(ctx, "发布意图已处理", "operation_id", ref.OperationID, "dispatch_id", ref.DispatchID, "sequence", ref.Sequence, "claim_outcome", outcome)
	return nil
}

// safeLogger 不透传框架原始连接错误；业务关联信息由上面的受控日志单独记录。
type safeLogger struct{ logger *slog.Logger }

var redisLogOnce sync.Once

type discardRedisLogger struct{}

func (discardRedisLogger) Printf(context.Context, string, ...interface{}) {}

func (l safeLogger) Debug(...interface{}) {}
func (l safeLogger) Info(...interface{})  {}
func (l safeLogger) Warn(...interface{})  { l.logger.Warn("Asynq 队列警告") }
func (l safeLogger) Error(...interface{}) { l.logger.Error("Asynq 队列暂时不可用") }
func (l safeLogger) Fatal(...interface{}) { l.logger.Error("Asynq 队列运行失败") }
