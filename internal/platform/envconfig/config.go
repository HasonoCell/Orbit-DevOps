package envconfig

import (
	"encoding/json"
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
	GitHubWebhook GitHubWebhook
	GitHubSource  GitHubSource
}

// GitHubWebhook 只在进程内保存验签材料；这些值不得进入数据库或日志。
type GitHubWebhook struct {
	Endpoints    map[string]GitHubWebhookSecrets
	MaxBodyBytes int64
}

type GitHubWebhookSecrets struct {
	CurrentSecret  string `json:"currentSecret"`
	PreviousSecret string `json:"previousSecret,omitempty"`
}

type GitHubSource struct {
	APIBaseURL string
	Token      string
	Timeout    time.Duration
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

// BuildWorker 将构建业务租约、队列运输和受限 Kubernetes Job 配置分开保存。
type BuildWorker struct {
	Address                 string
	DatabaseURL             string
	WorkerID                string
	PollInterval            time.Duration
	LeaseDuration           time.Duration
	BuildOperationTimeout   time.Duration
	MaximumAutomaticRetries int
	RetryBaseDelay          time.Duration
	KubeconfigPath          string
	KubernetesContext       string
	Namespace               string
	FieldManager            string
	GitImage                string
	BuildkitImage           string
	RegistrySecretName      string
	RegistryInsecure        bool
	DockerHubMirror         string
	DockerHubMirrorInsecure bool
	JobTTL                  time.Duration
	CPU                     string
	Memory                  string
	BuildQueue              ReleaseQueue
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
	githubWebhook, err := loadGitHubWebhook()
	if err != nil {
		return API{}, err
	}
	githubTimeout, err := duration("ORBITOPS_GITHUB_API_TIMEOUT", 5*time.Second)
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
			RegistryHost:    value("ORBITOPS_BUILD_REGISTRY_HOST", "orbitops-s4-registry.orbitops-s4-build.svc.cluster.local:5000"),
			RegistryPrefix:  value("ORBITOPS_BUILD_REGISTRY_PREFIX", "orbitops"),
		},
		GitHubWebhook: githubWebhook,
		GitHubSource:  GitHubSource{APIBaseURL: value("ORBITOPS_GITHUB_API_URL", "https://api.github.com"), Token: os.Getenv("ORBITOPS_GITHUB_API_TOKEN"), Timeout: githubTimeout},
	}, nil
}

func loadGitHubWebhook() (GitHubWebhook, error) {
	maximumBody, err := nonNegativeInteger("ORBITOPS_GITHUB_WEBHOOK_MAX_BODY_BYTES", 1024*1024)
	if err != nil || maximumBody < 1024 || maximumBody > 10*1024*1024 {
		return GitHubWebhook{}, errors.New("ORBITOPS_GITHUB_WEBHOOK_MAX_BODY_BYTES must be between 1024 and 10485760")
	}
	endpoints := map[string]GitHubWebhookSecrets{}
	configured := strings.TrimSpace(os.Getenv("ORBITOPS_GITHUB_WEBHOOK_ENDPOINTS"))
	if configured != "" {
		if err := json.Unmarshal([]byte(configured), &endpoints); err != nil {
			return GitHubWebhook{}, fmt.Errorf("parse ORBITOPS_GITHUB_WEBHOOK_ENDPOINTS: %w", err)
		}
	}
	for key, secrets := range endpoints {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(secrets.CurrentSecret) == "" {
			return GitHubWebhook{}, errors.New("every GitHub webhook endpoint requires a key and currentSecret")
		}
	}
	return GitHubWebhook{Endpoints: endpoints, MaxBodyBytes: int64(maximumBody)}, nil
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

// LoadBuildWorker 拒绝未固定 Digest 的运行镜像，并让队列超时覆盖完整构建窗口。
func LoadBuildWorker() (BuildWorker, error) {
	pollInterval, err := duration("ORBITOPS_BUILD_WORKER_POLL_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return BuildWorker{}, err
	}
	leaseDuration, err := duration("ORBITOPS_BUILD_WORKER_LEASE_DURATION", 15*time.Second)
	if err != nil {
		return BuildWorker{}, err
	}
	buildTimeout, err := duration("ORBITOPS_BUILD_OPERATION_TIMEOUT", 20*time.Minute)
	if err != nil {
		return BuildWorker{}, err
	}
	queue, err := loadBuildQueue(buildTimeout)
	if err != nil {
		return BuildWorker{}, err
	}
	maximumRetries, err := nonNegativeInteger("ORBITOPS_BUILD_MAX_AUTOMATIC_RETRIES", 2)
	if err != nil {
		return BuildWorker{}, err
	}
	retryBaseDelay, err := duration("ORBITOPS_BUILD_RETRY_BASE_DELAY", 5*time.Second)
	if err != nil {
		return BuildWorker{}, err
	}
	jobTTL, err := duration("ORBITOPS_BUILD_JOB_TTL", time.Hour)
	if err != nil {
		return BuildWorker{}, err
	}
	registryInsecure, err := boolean("ORBITOPS_BUILD_REGISTRY_INSECURE", false)
	if err != nil {
		return BuildWorker{}, err
	}
	dockerHubMirrorInsecure, err := boolean("ORBITOPS_BUILD_DOCKERHUB_MIRROR_INSECURE", false)
	if err != nil {
		return BuildWorker{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return BuildWorker{}, fmt.Errorf("read hostname: %w", err)
	}
	config := BuildWorker{
		Address: value("ORBITOPS_BUILD_WORKER_ADDRESS", "127.0.0.1:9092"), DatabaseURL: value("ORBITOPS_DATABASE_URL", defaultDatabaseURL),
		WorkerID: value("ORBITOPS_BUILD_WORKER_ID", hostname), PollInterval: pollInterval, LeaseDuration: leaseDuration,
		BuildOperationTimeout: buildTimeout, MaximumAutomaticRetries: maximumRetries, RetryBaseDelay: retryBaseDelay,
		KubeconfigPath: value("ORBITOPS_KUBECONFIG", clientcmd.RecommendedHomeFile), KubernetesContext: value("ORBITOPS_KUBERNETES_CONTEXT", "kind-orbitops-s1"),
		Namespace: value("ORBITOPS_BUILD_NAMESPACE", "orbitops-s4-build"), FieldManager: value("ORBITOPS_BUILD_FIELD_MANAGER", "orbitops-build-worker"),
		GitImage:           value("ORBITOPS_BUILD_GIT_IMAGE", "alpine/git:v2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"),
		BuildkitImage:      value("ORBITOPS_BUILDKIT_IMAGE", "moby/buildkit:v0.33.0-rootless@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef"),
		RegistrySecretName: os.Getenv("ORBITOPS_BUILD_REGISTRY_SECRET"), RegistryInsecure: registryInsecure, JobTTL: jobTTL,
		DockerHubMirror: os.Getenv("ORBITOPS_BUILD_DOCKERHUB_MIRROR"), DockerHubMirrorInsecure: dockerHubMirrorInsecure,
		CPU: value("ORBITOPS_BUILD_CPU", "1"), Memory: value("ORBITOPS_BUILD_MEMORY", "1Gi"), BuildQueue: queue,
	}
	for name, image := range map[string]string{"ORBITOPS_BUILD_GIT_IMAGE": config.GitImage, "ORBITOPS_BUILDKIT_IMAGE": config.BuildkitImage} {
		parts := strings.Split(image, "@sha256:")
		if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 64 {
			return BuildWorker{}, fmt.Errorf("%s must be pinned by sha256 digest", name)
		}
	}
	return config, nil
}

func loadBuildQueue(buildTimeout time.Duration) (ReleaseQueue, error) {
	config := ReleaseQueue{RedisAddress: value("ORBITOPS_REDIS_ADDRESS", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("ORBITOPS_REDIS_USERNAME"), RedisPassword: os.Getenv("ORBITOPS_REDIS_PASSWORD"),
		Name: value("ORBITOPS_BUILD_QUEUE_NAME", "orbitops-build")}
	var err error
	config.RedisDB, err = nonNegativeInteger("ORBITOPS_REDIS_DB", 0)
	if err != nil {
		return ReleaseQueue{}, err
	}
	config.Concurrency, err = nonNegativeInteger("ORBITOPS_BUILD_QUEUE_CONCURRENCY", 2)
	if err != nil || config.Concurrency < 1 || config.Concurrency > 100 {
		return ReleaseQueue{}, errors.New("ORBITOPS_BUILD_QUEUE_CONCURRENCY must be between 1 and 100")
	}
	for _, item := range []struct {
		name     string
		fallback time.Duration
		target   *time.Duration
	}{
		{"ORBITOPS_BUILD_QUEUE_REPAIR_INTERVAL", 5 * time.Second, &config.RepairInterval},
		{"ORBITOPS_BUILD_QUEUE_CONSUMPTION_GRACE", 30 * time.Second, &config.ConsumptionGrace},
		{"ORBITOPS_BUILD_QUEUE_TASK_TIMEOUT", buildTimeout + time.Minute, &config.TaskTimeout},
		{"ORBITOPS_BUILD_QUEUE_SHUTDOWN_TIMEOUT", 30 * time.Second, &config.ShutdownTimeout},
	} {
		*item.target, err = duration(item.name, item.fallback)
		if err != nil {
			return ReleaseQueue{}, err
		}
	}
	if config.TaskTimeout < buildTimeout+time.Minute {
		return ReleaseQueue{}, errors.New("ORBITOPS_BUILD_QUEUE_TASK_TIMEOUT must exceed build operation timeout by at least 1m")
	}
	return config, nil
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
