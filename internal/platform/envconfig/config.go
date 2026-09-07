package envconfig

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

const defaultDatabaseURL = "postgres://orbitops:orbitops@127.0.0.1:5432/orbitops?sslmode=disable"

type Kubernetes struct {
	KubeconfigPath string
	Context        string
	ClusterRef     string
	Namespace      string
	FieldManager   string
	PollInterval   time.Duration
}

type API struct {
	Address       string
	DatabaseURL   string
	ActorID       string
	MigrateOnBoot bool
	Kubernetes    Kubernetes
}

type Worker struct {
	Address                 string
	DatabaseURL             string
	WorkerID                string
	PollInterval            time.Duration
	LeaseDuration           time.Duration
	OperationTimeout        time.Duration
	MaximumAutomaticRetries int
	RetryBaseDelay          time.Duration
	Kubernetes              Kubernetes
	Queue                   Queue
}

// Queue 只配置消息运输；业务租约、重试预算仍由 Worker 与 Operation 管理。
type Queue struct {
	RedisAddress     string
	RedisUsername    string
	RedisPassword    string
	RedisDB          int
	Concurrency      int
	Name             string
	RepairInterval   time.Duration
	ConsumptionGrace time.Duration
	TaskTimeout      time.Duration
	ShutdownTimeout  time.Duration
}

// loadQueue 固定运输并发和时间边界；读取密钥但不校验连接，API 因此不依赖 Redis。
func loadQueue(operationTimeout time.Duration) (Queue, error) {
	config := Queue{RedisAddress: value("ORBITOPS_REDIS_ADDRESS", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("ORBITOPS_REDIS_USERNAME"), RedisPassword: os.Getenv("ORBITOPS_REDIS_PASSWORD"),
		Name: value("ORBITOPS_QUEUE_NAME", "orbitops-release")}
	var err error
	config.RedisDB, err = nonNegativeInteger("ORBITOPS_REDIS_DB", 0)
	if err != nil {
		return Queue{}, err
	}
	config.Concurrency, err = nonNegativeInteger("ORBITOPS_QUEUE_CONCURRENCY", 4)
	if err != nil {
		return Queue{}, err
	}
	if config.Concurrency < 1 || config.Concurrency > 100 {
		return Queue{}, errors.New("ORBITOPS_QUEUE_CONCURRENCY must be between 1 and 100")
	}
	for _, item := range []struct {
		name     string
		fallback time.Duration
		target   *time.Duration
	}{
		{"ORBITOPS_QUEUE_REPAIR_INTERVAL", 5 * time.Second, &config.RepairInterval},
		{"ORBITOPS_QUEUE_CONSUMPTION_GRACE", 30 * time.Second, &config.ConsumptionGrace},
		{"ORBITOPS_QUEUE_TASK_TIMEOUT", operationTimeout + 30*time.Second, &config.TaskTimeout},
		{"ORBITOPS_QUEUE_SHUTDOWN_TIMEOUT", 15 * time.Second, &config.ShutdownTimeout},
	} {
		*item.target, err = duration(item.name, item.fallback)
		if err != nil {
			return Queue{}, err
		}
	}
	// 外层超时必须给输入读取和结果提交留余量，不能抢先截断业务超时。
	if config.TaskTimeout < operationTimeout+30*time.Second {
		return Queue{}, errors.New("ORBITOPS_QUEUE_TASK_TIMEOUT must exceed operation timeout by at least 30s")
	}
	return config, nil
}

func LoadAPI() (API, error) {
	kubernetes, err := loadKubernetes()
	if err != nil {
		return API{}, err
	}
	migrateOnBoot, err := boolean("ORBITOPS_MIGRATE_ON_BOOT", true)
	if err != nil {
		return API{}, err
	}
	return API{
		Address:       value("ORBITOPS_API_ADDRESS", "127.0.0.1:8080"),
		DatabaseURL:   value("ORBITOPS_DATABASE_URL", defaultDatabaseURL),
		ActorID:       value("ORBITOPS_ACTOR_ID", "local-developer"),
		MigrateOnBoot: migrateOnBoot,
		Kubernetes:    kubernetes,
	}, nil
}

// LoadWorker 分开加载业务租约与运输参数，拒绝会提前截断业务执行的队列超时。
func LoadWorker() (Worker, error) {
	kubernetes, err := loadKubernetes()
	if err != nil {
		return Worker{}, err
	}
	pollInterval, err := duration("ORBITOPS_WORKER_POLL_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return Worker{}, err
	}
	leaseDuration, err := duration("ORBITOPS_WORKER_LEASE_DURATION", 10*time.Second)
	if err != nil {
		return Worker{}, err
	}
	operationTimeout, err := duration("ORBITOPS_OPERATION_TIMEOUT", 2*time.Minute)
	if err != nil {
		return Worker{}, err
	}
	queue, err := loadQueue(operationTimeout)
	if err != nil {
		return Worker{}, err
	}
	maximumAutomaticRetries, err := nonNegativeInteger(
		"ORBITOPS_MAX_AUTOMATIC_RETRIES",
		2,
	)
	if err != nil {
		return Worker{}, err
	}
	retryBaseDelay, err := duration("ORBITOPS_RETRY_BASE_DELAY", time.Second)
	if err != nil {
		return Worker{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return Worker{}, fmt.Errorf("read hostname: %w", err)
	}
	return Worker{
		Address:                 value("ORBITOPS_WORKER_ADDRESS", "127.0.0.1:9091"),
		DatabaseURL:             value("ORBITOPS_DATABASE_URL", defaultDatabaseURL),
		WorkerID:                value("ORBITOPS_WORKER_ID", hostname),
		PollInterval:            pollInterval,
		LeaseDuration:           leaseDuration,
		OperationTimeout:        operationTimeout,
		MaximumAutomaticRetries: maximumAutomaticRetries,
		RetryBaseDelay:          retryBaseDelay,
		Kubernetes:              kubernetes,
		Queue:                   queue,
	}, nil
}

func loadKubernetes() (Kubernetes, error) {
	pollInterval, err := duration("ORBITOPS_KUBERNETES_POLL_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return Kubernetes{}, err
	}
	return Kubernetes{
		KubeconfigPath: value("ORBITOPS_KUBECONFIG", clientcmd.RecommendedHomeFile),
		Context:        value("ORBITOPS_KUBERNETES_CONTEXT", "kind-orbitops-s1"),
		ClusterRef:     value("ORBITOPS_CLUSTER_REF", "kind-orbitops-s1"),
		Namespace:      value("ORBITOPS_NAMESPACE", "orbitops-s1"),
		FieldManager:   value("ORBITOPS_FIELD_MANAGER", "orbitops-worker"),
		PollInterval:   pollInterval,
	}, nil
}

func value(name string, fallback string) string {
	if configured := os.Getenv(name); configured != "" {
		return configured
	}
	return fallback
}

func duration(name string, fallback time.Duration) (time.Duration, error) {
	configured := os.Getenv(name)
	if configured == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(configured)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return parsed, nil
}

func boolean(name string, fallback bool) (bool, error) {
	configured := os.Getenv(name)
	if configured == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(configured)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func nonNegativeInteger(name string, fallback int) (int, error) {
	configured := os.Getenv(name)
	if configured == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(configured)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return parsed, nil
}

func ValidateKubernetes(config Kubernetes) error {
	if config.KubeconfigPath == "" || config.Context == "" || config.ClusterRef == "" ||
		config.Namespace == "" || config.FieldManager == "" {
		return errors.New("Kubernetes configuration is incomplete")
	}
	return nil
}
