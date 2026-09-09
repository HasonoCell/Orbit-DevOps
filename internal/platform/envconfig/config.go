package envconfig

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	SourceBuild   SourceBuild
}

// SourceBuild 是 API 用来冻结构建目标的受控配置；用户请求不能覆盖这些边界。
type SourceBuild struct {
	AllowedGitHosts []string
	Platform        string
	RegistryHost    string
	RegistryPrefix  string
}

type ReleaseWorker struct {
	Address                 string
	DatabaseURL             string
	WorkerID                string
	PollInterval            time.Duration
	LeaseDuration           time.Duration
	ReleaseOperationTimeout time.Duration
	MaximumAutomaticRetries int
	RetryBaseDelay          time.Duration
	Kubernetes              Kubernetes
	ReleaseQueue            ReleaseQueue
}

// ReleaseQueue 只配置消息运输；业务租约、重试预算仍由 ReleaseWorker 与 ReleaseOperation 管理。
type ReleaseQueue struct {
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

// loadReleaseQueue 固定运输并发和时间边界；读取密钥但不校验连接，API 因此不依赖 Redis。
func loadReleaseQueue(releaseOperationTimeout time.Duration) (ReleaseQueue, error) {
	config := ReleaseQueue{RedisAddress: value("ORBITOPS_REDIS_ADDRESS", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("ORBITOPS_REDIS_USERNAME"), RedisPassword: os.Getenv("ORBITOPS_REDIS_PASSWORD"),
		Name: value("ORBITOPS_RELEASE_QUEUE_NAME", "orbitops-release")}
	var err error
	config.RedisDB, err = nonNegativeInteger("ORBITOPS_REDIS_DB", 0)
	if err != nil {
		return ReleaseQueue{}, err
	}
	config.Concurrency, err = nonNegativeInteger("ORBITOPS_RELEASE_QUEUE_CONCURRENCY", 4)
	if err != nil {
		return ReleaseQueue{}, err
	}
	if config.Concurrency < 1 || config.Concurrency > 100 {
		return ReleaseQueue{}, errors.New("ORBITOPS_RELEASE_QUEUE_CONCURRENCY must be between 1 and 100")
	}
	for _, item := range []struct {
		name     string
		fallback time.Duration
		target   *time.Duration
	}{
		{"ORBITOPS_RELEASE_QUEUE_REPAIR_INTERVAL", 5 * time.Second, &config.RepairInterval},
		{"ORBITOPS_RELEASE_QUEUE_CONSUMPTION_GRACE", 30 * time.Second, &config.ConsumptionGrace},
		{"ORBITOPS_RELEASE_QUEUE_TASK_TIMEOUT", releaseOperationTimeout + 30*time.Second, &config.TaskTimeout},
		{"ORBITOPS_RELEASE_QUEUE_SHUTDOWN_TIMEOUT", 15 * time.Second, &config.ShutdownTimeout},
	} {
		*item.target, err = duration(item.name, item.fallback)
		if err != nil {
			return ReleaseQueue{}, err
		}
	}
	// 外层超时必须给输入读取和结果提交留余量，不能抢先截断业务超时。
	if config.TaskTimeout < releaseOperationTimeout+30*time.Second {
		return ReleaseQueue{}, errors.New("ORBITOPS_RELEASE_QUEUE_TASK_TIMEOUT must exceed release operation timeout by at least 30s")
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
		SourceBuild: SourceBuild{
			AllowedGitHosts: commaSeparated("ORBITOPS_BUILD_GIT_ALLOWED_HOSTS", []string{"github.com", "gitea.com"}),
			Platform:        value("ORBITOPS_BUILD_PLATFORM", "linux/amd64"),
			RegistryHost:    value("ORBITOPS_BUILD_REGISTRY_HOST", "127.0.0.1:5001"),
			RegistryPrefix:  value("ORBITOPS_BUILD_REGISTRY_PREFIX", "orbitops"),
		},
	}, nil
}

// LoadReleaseWorker 分开加载业务租约与运输参数，拒绝会提前截断业务执行的队列超时。
func LoadReleaseWorker() (ReleaseWorker, error) {
	kubernetes, err := loadKubernetes()
	if err != nil {
		return ReleaseWorker{}, err
	}
	pollInterval, err := duration("ORBITOPS_RELEASE_WORKER_POLL_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return ReleaseWorker{}, err
	}
	leaseDuration, err := duration("ORBITOPS_RELEASE_WORKER_LEASE_DURATION", 10*time.Second)
	if err != nil {
		return ReleaseWorker{}, err
	}
	releaseOperationTimeout, err := duration("ORBITOPS_RELEASE_OPERATION_TIMEOUT", 2*time.Minute)
	if err != nil {
		return ReleaseWorker{}, err
	}
	queue, err := loadReleaseQueue(releaseOperationTimeout)
	if err != nil {
		return ReleaseWorker{}, err
	}
	maximumAutomaticRetries, err := nonNegativeInteger(
		"ORBITOPS_RELEASE_MAX_AUTOMATIC_RETRIES",
		2,
	)
	if err != nil {
		return ReleaseWorker{}, err
	}
	retryBaseDelay, err := duration("ORBITOPS_RELEASE_RETRY_BASE_DELAY", time.Second)
	if err != nil {
		return ReleaseWorker{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return ReleaseWorker{}, fmt.Errorf("read hostname: %w", err)
	}
	return ReleaseWorker{
		Address:                 value("ORBITOPS_RELEASE_WORKER_ADDRESS", "127.0.0.1:9091"),
		DatabaseURL:             value("ORBITOPS_DATABASE_URL", defaultDatabaseURL),
		WorkerID:                value("ORBITOPS_RELEASE_WORKER_ID", hostname),
		PollInterval:            pollInterval,
		LeaseDuration:           leaseDuration,
		ReleaseOperationTimeout: releaseOperationTimeout,
		MaximumAutomaticRetries: maximumAutomaticRetries,
		RetryBaseDelay:          retryBaseDelay,
		Kubernetes:              kubernetes,
		ReleaseQueue:            queue,
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

func commaSeparated(name string, fallback []string) []string {
	configured := os.Getenv(name)
	if configured == "" {
		return append([]string(nil), fallback...)
	}
	items := make([]string, 0)
	for _, item := range strings.Split(configured, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
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
