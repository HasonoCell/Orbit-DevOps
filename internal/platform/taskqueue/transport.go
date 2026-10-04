// Package taskqueue 只拥有 Redis/Asynq 运输与生命周期，不读取业务数据库或决定执行权。
package taskqueue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	redisclient "github.com/redis/go-redis/v9"
)

var (
	ErrUnavailable        = errors.New("queue_unavailable")
	ErrStopping           = errors.New("queue_stopping")
	ErrHandlerInterrupted = errors.New("queue_handler_interrupted")
)

// Config 显式保留不同领域的运输重试策略；OnPanic 只接收通知，不接收原始 panic 内容。
type Config struct {
	RedisAddress, RedisUsername, RedisPassword string
	RedisDB                                    int
	Queue                                      string
	Concurrency, MaxRetry                      int
	PollInterval, TaskTimeout, ShutdownTimeout time.Duration
	RetryDelay                                 asynq.RetryDelayFunc
	Logger                                     *slog.Logger
	OnPanic                                    func()
}

// Transport 一个实例只启动一次。Close 可重复调用，且等待全部已登记处理器退出。
type Transport struct {
	config                   Config
	connection               *redisclient.Client
	client                   *asynq.Client
	server                   *asynq.Server
	mu                       sync.Mutex
	started, closed, closing bool
	workers                  sync.WaitGroup
	cancel                   context.CancelFunc
	closeOnce                sync.Once
	closeErr                 error
}

var redisLogOnce sync.Once

func New(config Config) (*Transport, error) {
	if config.RedisAddress == "" || config.RedisDB < 0 || strings.TrimSpace(config.Queue) == "" ||
		config.Concurrency < 1 || config.Concurrency > 100 || config.MaxRetry < 0 ||
		config.PollInterval <= 0 || config.TaskTimeout <= 0 || config.ShutdownTimeout <= 0 {
		return nil, errors.New("invalid task queue configuration")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// go-redis 的 Logger 是进程级全局变量，只初始化一次以避免多 Service 装配时的竞争。
	redisLogOnce.Do(func() { redisclient.SetLogger(discardRedisLogger{}) })
	connection := redisclient.NewClient(&redisclient.Options{Addr: config.RedisAddress,
		Username: config.RedisUsername, Password: config.RedisPassword, DB: config.RedisDB,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		ContextTimeoutEnabled: true, MaxRetries: -1})
	return &Transport{config: config, connection: connection, client: asynq.NewClientFromRedisClient(connection)}, nil
}

// Send 有一秒的独立预算，返回安全运输结果；入队成功不等于领域命令已经完成。
func (q *Transport) Send(ctx context.Context, task *asynq.Task, availableAt time.Time) (string, error) {
	send, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	info, err := q.client.EnqueueContext(send, task, asynq.Queue(q.config.Queue),
		asynq.ProcessAt(availableAt), asynq.MaxRetry(q.config.MaxRetry), asynq.Timeout(q.config.TaskTimeout))
	if err != nil {
		return "", ErrUnavailable
	}
	return info.ID, nil
}

func (q *Transport) Ping(ctx context.Context) error {
	if q.connection.Ping(ctx).Err() != nil {
		return ErrUnavailable
	}
	return nil
}

// Start 与 Close 串行装配。消费者的 Context 独立于发布循环，退出时先留出排空窗口。
func (q *Transport) Start(handler asynq.Handler) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrStopping
	}
	if q.started {
		return errors.New("queue_already_started")
	}
	if handler == nil {
		return errors.New("queue_handler_required")
	}
	q.started = true
	workerContext, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	q.server = asynq.NewServer(asynq.RedisClientOpt{Addr: q.config.RedisAddress,
		Username: q.config.RedisUsername, Password: q.config.RedisPassword, DB: q.config.RedisDB,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second}, asynq.Config{
		Concurrency: q.config.Concurrency, Queues: map[string]int{q.config.Queue: 1},
		BaseContext:       func() context.Context { return workerContext },
		TaskCheckInterval: q.config.PollInterval, DelayedTaskCheckInterval: q.config.PollInterval,
		ShutdownTimeout: q.config.ShutdownTimeout, RetryDelayFunc: q.config.RetryDelay,
		Logger: safeLogger{q.config.Logger}, LogLevel: asynq.ErrorLevel,
	})
	if err := q.server.Start(asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) (result error) {
		q.mu.Lock()
		if q.closing {
			q.mu.Unlock()
			return ErrStopping
		}
		q.workers.Add(1)
		q.mu.Unlock()
		defer q.workers.Done()
		// 在框架之前截断 panic，避免任意内容进入日志或 Redis 的错误归档。
		defer func() {
			if recover() != nil {
				result = ErrHandlerInterrupted
				q.config.Logger.WarnContext(ctx, "队列处理器被中断")
				if q.config.OnPanic != nil {
					q.config.OnPanic()
				}
			}
		}()
		return handler.ProcessTask(ctx, task)
	})); err != nil {
		cancel()
		q.closed, q.closing = true, true
		q.closeOnce.Do(func() { _ = q.connection.Close() })
		return errors.New("queue_start_failed")
	}
	return nil
}

// Stop 停止领取新任务；领域侧随后停止并等待自己的投递/维护循环，再调用 Close。
func (q *Transport) Stop() {
	q.mu.Lock()
	server := q.server
	q.mu.Unlock()
	if server != nil {
		server.Stop()
	}
}

// Close 排空后取消残留处理器并等待响应取消。数据库所有者必须在它返回后再关池。
func (q *Transport) Close() error {
	q.closeOnce.Do(func() {
		q.mu.Lock()
		q.closed = true
		server := q.server
		q.mu.Unlock()
		if server != nil {
			server.Stop()
			server.Shutdown()
		}
		q.mu.Lock()
		q.closing = true
		if q.cancel != nil {
			q.cancel()
		}
		q.mu.Unlock()
		q.workers.Wait()
		if q.connection.Close() != nil {
			q.closeErr = errors.New("queue_close_failed")
		}
	})
	return q.closeErr
}

// ShortRetryDelay 保留 Release/Build 原有退避；事件运输仍采用 Asynq 默认策略。
func ShortRetryDelay(n int, _ error, _ *asynq.Task) time.Duration {
	return min(time.Second*time.Duration(1<<min(n, 5)), 30*time.Second)
}

type discardRedisLogger struct{}

func (discardRedisLogger) Printf(context.Context, string, ...interface{}) {}

type safeLogger struct{ logger *slog.Logger }

func (l safeLogger) Debug(...interface{}) {}
func (l safeLogger) Info(...interface{})  {}
func (l safeLogger) Warn(...interface{})  { l.logger.Warn("Asynq 队列警告") }
func (l safeLogger) Error(...interface{}) { l.logger.Error("Asynq 队列暂时不可用") }
func (l safeLogger) Fatal(...interface{}) { l.logger.Error("Asynq 队列运行失败") }
