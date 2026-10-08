package envconfig

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/kubeconnection"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultDatabaseURL = "postgres://orbitdevops:orbitdevops@127.0.0.1:5432/orbitdevops?sslmode=disable"

type Kubernetes struct {
	Connection   kubeconnection.Config
	ClusterRef   string
	Namespace    string
	FieldManager string
	PollInterval time.Duration
}

type API struct {
	Tracing       observability.TraceConfig
	DatabasePool  database.PoolConfig
	Address       string
	DatabaseURL   string
	MigrateOnBoot bool
	Browser       Browser
	OIDC          OIDC
	Kubernetes    Kubernetes
	SourceBuild   SourceBuild
	GitHubWebhook GitHubWebhook
	GitHubSource  GitHubSource
	Access        Access
}

// Access 只接受由部署者设置的 GatewayClass 与 Issuer 策略，不接受请求中的原始 K8s 引用。
type Access struct {
	GatewayClassName string
	IssuerPolicies   map[string]AccessIssuerPolicy
}

type AccessIssuerPolicy struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type GatewayWorker struct {
	Tracing             observability.TraceConfig
	DatabasePool        database.PoolConfig
	Address             string
	DatabaseURL         string
	Kubernetes          Kubernetes
	Access              Access
	Queue               ReleaseQueue
	PollInterval        time.Duration
	MaintenanceInterval time.Duration
	LeaseDuration       time.Duration
}

type OIDC struct {
	RedirectURL   string
	ClientSecrets map[string]string
	EncryptionKey []byte
	HTTPTimeout   time.Duration
}

type Browser struct {
	ExternalURL       string
	TrustedOrigins    []string
	AllowLoopbackHTTP bool
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
	Tracing                 observability.TraceConfig
	DatabasePool            database.PoolConfig
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
	KubernetesConnection    kubeconnection.Config
	Tracing                 observability.TraceConfig
	DatabasePool            database.PoolConfig
	Address                 string
	DatabaseURL             string
	WorkerID                string
	PollInterval            time.Duration
	LeaseDuration           time.Duration
	BuildOperationTimeout   time.Duration
	MaximumAutomaticRetries int
	RetryBaseDelay          time.Duration
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

// PipelineWorker 只配置数据库、内部事件运输与 GitHub 出站访问，不包含集群或 Registry 凭据。
type PipelineWorker struct {
	Tracing              observability.TraceConfig
	DatabasePool         database.PoolConfig
	Address              string
	DatabaseURL          string
	PollInterval         time.Duration
	MaintenanceInterval  time.Duration
	SourceRecoveryWindow time.Duration
	SourceRetryBaseDelay time.Duration
	SourceBuild          SourceBuild
	GitHubSource         GitHubSource
	Queue                ReleaseQueue
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
	config := ReleaseQueue{RedisAddress: value("ORBIT_DEVOPS_REDIS_ADDRESS", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("ORBIT_DEVOPS_REDIS_USERNAME"), RedisPassword: os.Getenv("ORBIT_DEVOPS_REDIS_PASSWORD"),
		Name: value("ORBIT_DEVOPS_RELEASE_QUEUE_NAME", "orbit-devops-release")}
	var err error
	config.RedisDB, err = nonNegativeInteger("ORBIT_DEVOPS_REDIS_DB", 0)
	if err != nil {
		return ReleaseQueue{}, err
	}
	config.Concurrency, err = nonNegativeInteger("ORBIT_DEVOPS_RELEASE_QUEUE_CONCURRENCY", 4)
	if err != nil {
		return ReleaseQueue{}, err
	}
	if config.Concurrency < 1 || config.Concurrency > 100 {
		return ReleaseQueue{}, errors.New("ORBIT_DEVOPS_RELEASE_QUEUE_CONCURRENCY must be between 1 and 100")
	}
	for _, item := range []struct {
		name     string
		fallback time.Duration
		target   *time.Duration
	}{
		{"ORBIT_DEVOPS_RELEASE_QUEUE_REPAIR_INTERVAL", 5 * time.Second, &config.RepairInterval},
		{"ORBIT_DEVOPS_RELEASE_QUEUE_CONSUMPTION_GRACE", 30 * time.Second, &config.ConsumptionGrace},
		{"ORBIT_DEVOPS_RELEASE_QUEUE_TASK_TIMEOUT", releaseOperationTimeout + 30*time.Second, &config.TaskTimeout},
		{"ORBIT_DEVOPS_RELEASE_QUEUE_SHUTDOWN_TIMEOUT", 15 * time.Second, &config.ShutdownTimeout},
	} {
		*item.target, err = duration(item.name, item.fallback)
		if err != nil {
			return ReleaseQueue{}, err
		}
	}
	// 外层超时必须给输入读取和结果提交留余量，不能抢先截断业务超时。
	if config.TaskTimeout < releaseOperationTimeout+30*time.Second {
		return ReleaseQueue{}, errors.New("ORBIT_DEVOPS_RELEASE_QUEUE_TASK_TIMEOUT must exceed release operation timeout by at least 30s")
	}
	return config, nil
}

func LoadAPI() (API, error) {
	pool, err := loadDatabasePool(20)
	if err != nil {
		return API{}, err
	}
	tracing, err := loadTracing()
	if err != nil {
		return API{}, err
	}
	kubernetes, err := loadKubernetes()
	if err != nil {
		return API{}, err
	}
	githubWebhook, err := loadGitHubWebhook()
	if err != nil {
		return API{}, err
	}
	githubTimeout, err := duration("ORBIT_DEVOPS_GITHUB_API_TIMEOUT", 5*time.Second)
	if err != nil {
		return API{}, err
	}
	migrateOnBoot, err := boolean("ORBIT_DEVOPS_MIGRATE_ON_BOOT", true)
	if err != nil {
		return API{}, err
	}
	allowLoopbackHTTP, err := boolean("ORBIT_DEVOPS_ALLOW_LOOPBACK_HTTP", true)
	if err != nil {
		return API{}, err
	}
	oidc, err := loadOIDC()
	if err != nil {
		return API{}, err
	}
	accessConfig, err := loadAccess()
	if err != nil {
		return API{}, err
	}
	return API{
		DatabasePool:  pool,
		Tracing:       tracing,
		Address:       value("ORBIT_DEVOPS_API_ADDRESS", "127.0.0.1:8080"),
		DatabaseURL:   value("ORBIT_DEVOPS_DATABASE_URL", defaultDatabaseURL),
		MigrateOnBoot: migrateOnBoot,
		Browser: Browser{
			ExternalURL:       value("ORBIT_DEVOPS_EXTERNAL_URL", "http://127.0.0.1:5173"),
			TrustedOrigins:    commaSeparated("ORBIT_DEVOPS_TRUSTED_ORIGINS", nil),
			AllowLoopbackHTTP: allowLoopbackHTTP,
		},
		OIDC:          oidc,
		Kubernetes:    kubernetes,
		SourceBuild:   loadSourceBuild(),
		GitHubWebhook: githubWebhook,
		GitHubSource:  GitHubSource{APIBaseURL: value("ORBIT_DEVOPS_GITHUB_API_URL", "https://api.github.com"), Token: os.Getenv("ORBIT_DEVOPS_GITHUB_API_TOKEN"), Timeout: githubTimeout},
		Access:        accessConfig,
	}, nil
}

func loadAccess() (Access, error) {
	config := Access{GatewayClassName: strings.TrimSpace(os.Getenv("ORBIT_DEVOPS_GATEWAY_CLASS_NAME")), IssuerPolicies: map[string]AccessIssuerPolicy{}}
	if config.GatewayClassName != "" && len(validation.IsDNS1123Subdomain(config.GatewayClassName)) > 0 {
		return Access{}, errors.New("ORBIT_DEVOPS_GATEWAY_CLASS_NAME must be a Kubernetes DNS name")
	}
	if raw := strings.TrimSpace(os.Getenv("ORBIT_DEVOPS_ACCESS_ISSUER_POLICIES_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &config.IssuerPolicies); err != nil {
			return Access{}, errors.New("ORBIT_DEVOPS_ACCESS_ISSUER_POLICIES_JSON must be a JSON policy map")
		}
	}
	for key, policy := range config.IssuerPolicies {
		if len(validation.IsDNS1123Label(key)) > 0 || (policy.Kind != "Issuer" && policy.Kind != "ClusterIssuer") || len(validation.IsDNS1123Subdomain(policy.Name)) > 0 {
			return Access{}, errors.New("invalid access issuer policy")
		}
	}
	return config, nil
}

// LoadGatewayWorker 分离入口写入进程，不把集群写权限授予 Pipeline Worker。
func LoadGatewayWorker() (GatewayWorker, error) {
	pool, err := loadDatabasePool(8)
	if err != nil {
		return GatewayWorker{}, err
	}
	tracing, err := loadTracing()
	if err != nil {
		return GatewayWorker{}, err
	}
	kubernetes, err := loadKubernetes()
	if err != nil {
		return GatewayWorker{}, err
	}
	accessConfig, err := loadAccess()
	if err != nil {
		return GatewayWorker{}, err
	}
	if accessConfig.GatewayClassName == "" {
		return GatewayWorker{}, errors.New("ORBIT_DEVOPS_GATEWAY_CLASS_NAME is required")
	}
	poll, err := duration("ORBIT_DEVOPS_GATEWAY_WORKER_POLL_INTERVAL", time.Second)
	if err != nil {
		return GatewayWorker{}, err
	}
	maintenance, err := duration("ORBIT_DEVOPS_GATEWAY_MAINTENANCE_INTERVAL", time.Minute)
	if err != nil {
		return GatewayWorker{}, err
	}
	lease, err := duration("ORBIT_DEVOPS_GATEWAY_LEASE_DURATION", 45*time.Second)
	if err != nil {
		return GatewayWorker{}, err
	}
	queue, err := loadGatewayQueue()
	if err != nil {
		return GatewayWorker{}, err
	}
	if lease >= queue.TaskTimeout {
		return GatewayWorker{}, errors.New("gateway lease must be shorter than task timeout")
	}
	return GatewayWorker{DatabasePool: pool, Tracing: tracing, Address: value("ORBIT_DEVOPS_GATEWAY_WORKER_ADDRESS", "127.0.0.1:9094"),
		DatabaseURL: value("ORBIT_DEVOPS_DATABASE_URL", defaultDatabaseURL), Kubernetes: kubernetes,
		Access: accessConfig, Queue: queue, PollInterval: poll, MaintenanceInterval: maintenance,
		LeaseDuration: lease}, nil
}

func loadGatewayQueue() (ReleaseQueue, error) {
	config := ReleaseQueue{RedisAddress: value("ORBIT_DEVOPS_REDIS_ADDRESS", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("ORBIT_DEVOPS_REDIS_USERNAME"), RedisPassword: os.Getenv("ORBIT_DEVOPS_REDIS_PASSWORD"),
		Name: value("ORBIT_DEVOPS_GATEWAY_QUEUE_NAME", "orbit-devops-gateway")}
	var err error
	config.RedisDB, err = nonNegativeInteger("ORBIT_DEVOPS_REDIS_DB", 0)
	if err != nil {
		return ReleaseQueue{}, err
	}
	config.Concurrency, err = nonNegativeInteger("ORBIT_DEVOPS_GATEWAY_QUEUE_CONCURRENCY", 2)
	if err != nil || config.Concurrency < 1 || config.Concurrency > 100 {
		return ReleaseQueue{}, errors.New("invalid gateway queue concurrency")
	}
	config.ConsumptionGrace, err = duration("ORBIT_DEVOPS_GATEWAY_CONSUMPTION_GRACE", 30*time.Second)
	if err != nil {
		return ReleaseQueue{}, err
	}
	config.TaskTimeout, err = duration("ORBIT_DEVOPS_GATEWAY_TASK_TIMEOUT", 60*time.Second)
	if err != nil {
		return ReleaseQueue{}, err
	}
	config.ShutdownTimeout, err = duration("ORBIT_DEVOPS_GATEWAY_SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return ReleaseQueue{}, err
	}
	return config, nil
}

func loadOIDC() (OIDC, error) {
	timeout, err := duration("ORBIT_DEVOPS_OIDC_HTTP_TIMEOUT", 10*time.Second)
	if err != nil {
		return OIDC{}, err
	}
	// OIDC callback 由 API 处理，不能从前端 ExternalURL 推导；完成后再跳转到固定前端入口。
	config := OIDC{RedirectURL: value("ORBIT_DEVOPS_OIDC_REDIRECT_URL", "http://127.0.0.1:8080/api/v1/auth/oidc/callback"),
		ClientSecrets: map[string]string{}, HTTPTimeout: timeout}
	if raw := strings.TrimSpace(os.Getenv("ORBIT_DEVOPS_OIDC_CLIENT_SECRETS_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &config.ClientSecrets); err != nil {
			return OIDC{}, errors.New("ORBIT_DEVOPS_OIDC_CLIENT_SECRETS_JSON must be a JSON string map")
		}
	}
	if raw := strings.TrimSpace(os.Getenv("ORBIT_DEVOPS_OIDC_ENCRYPTION_KEY")); raw != "" {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
		if err != nil || len(decoded) != 32 {
			return OIDC{}, errors.New("ORBIT_DEVOPS_OIDC_ENCRYPTION_KEY must be raw URL base64 for 32 bytes")
		}
		config.EncryptionKey = decoded
	}
	return config, nil
}

// LoadPipelineWorker 让自动编排进程与 Build/Release Worker 保持权限隔离。
func LoadPipelineWorker() (PipelineWorker, error) {
	pool, err := loadDatabasePool(12)
	if err != nil {
		return PipelineWorker{}, err
	}
	tracing, err := loadTracing()
	if err != nil {
		return PipelineWorker{}, err
	}
	poll, err := duration("ORBIT_DEVOPS_PIPELINE_WORKER_POLL_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return PipelineWorker{}, err
	}
	maintenance, err := duration("ORBIT_DEVOPS_PIPELINE_MAINTENANCE_INTERVAL", time.Minute)
	if err != nil {
		return PipelineWorker{}, err
	}
	recoveryWindow, err := duration("ORBIT_DEVOPS_PIPELINE_SOURCE_RECOVERY_WINDOW", 15*time.Minute)
	if err != nil {
		return PipelineWorker{}, err
	}
	retryDelay, err := duration("ORBIT_DEVOPS_PIPELINE_SOURCE_RETRY_BASE_DELAY", 5*time.Second)
	if err != nil {
		return PipelineWorker{}, err
	}
	githubTimeout, err := duration("ORBIT_DEVOPS_GITHUB_API_TIMEOUT", 5*time.Second)
	if err != nil {
		return PipelineWorker{}, err
	}
	queue, err := loadPipelineQueue()
	if err != nil {
		return PipelineWorker{}, err
	}
	return PipelineWorker{
		DatabasePool: pool, Tracing: tracing,
		Address:     value("ORBIT_DEVOPS_PIPELINE_WORKER_ADDRESS", "127.0.0.1:9093"),
		DatabaseURL: value("ORBIT_DEVOPS_DATABASE_URL", defaultDatabaseURL), PollInterval: poll,
		MaintenanceInterval: maintenance, SourceRecoveryWindow: recoveryWindow,
		SourceRetryBaseDelay: retryDelay, SourceBuild: loadSourceBuild(), Queue: queue,
		GitHubSource: GitHubSource{APIBaseURL: value("ORBIT_DEVOPS_GITHUB_API_URL", "https://api.github.com"), Token: os.Getenv("ORBIT_DEVOPS_GITHUB_API_TOKEN"), Timeout: githubTimeout},
	}, nil
}

func loadPipelineQueue() (ReleaseQueue, error) {
	config := ReleaseQueue{RedisAddress: value("ORBIT_DEVOPS_REDIS_ADDRESS", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("ORBIT_DEVOPS_REDIS_USERNAME"), RedisPassword: os.Getenv("ORBIT_DEVOPS_REDIS_PASSWORD"),
		Name: value("ORBIT_DEVOPS_PIPELINE_QUEUE_NAME", "orbit-devops-pipeline")}
	var err error
	config.RedisDB, err = nonNegativeInteger("ORBIT_DEVOPS_REDIS_DB", 0)
	if err != nil {
		return ReleaseQueue{}, err
	}
	config.Concurrency, err = nonNegativeInteger("ORBIT_DEVOPS_PIPELINE_QUEUE_CONCURRENCY", 4)
	if err != nil || config.Concurrency < 1 || config.Concurrency > 100 {
		return ReleaseQueue{}, errors.New("ORBIT_DEVOPS_PIPELINE_QUEUE_CONCURRENCY must be between 1 and 100")
	}
	for _, item := range []struct {
		name     string
		fallback time.Duration
		target   *time.Duration
	}{
		{"ORBIT_DEVOPS_PIPELINE_QUEUE_CONSUMPTION_GRACE", 30 * time.Second, &config.ConsumptionGrace},
		{"ORBIT_DEVOPS_PIPELINE_QUEUE_TASK_TIMEOUT", 30 * time.Second, &config.TaskTimeout},
		{"ORBIT_DEVOPS_PIPELINE_QUEUE_SHUTDOWN_TIMEOUT", 15 * time.Second, &config.ShutdownTimeout},
	} {
		*item.target, err = duration(item.name, item.fallback)
		if err != nil {
			return ReleaseQueue{}, err
		}
	}
	config.RepairInterval = time.Minute
	return config, nil
}

func loadSourceBuild() SourceBuild {
	return SourceBuild{
		AllowedGitHosts: commaSeparated("ORBIT_DEVOPS_BUILD_GIT_ALLOWED_HOSTS", []string{"github.com", "gitea.com"}),
		Platform:        value("ORBIT_DEVOPS_BUILD_PLATFORM", "linux/amd64"),
		RegistryHost:    value("ORBIT_DEVOPS_BUILD_REGISTRY_HOST", "orbit-devops-s4-registry.orbit-devops-s4-build.svc.cluster.local:5000"),
		RegistryPrefix:  value("ORBIT_DEVOPS_BUILD_REGISTRY_PREFIX", "orbit-devops"),
	}
}

func loadGitHubWebhook() (GitHubWebhook, error) {
	maximumBody, err := nonNegativeInteger("ORBIT_DEVOPS_GITHUB_WEBHOOK_MAX_BODY_BYTES", 1024*1024)
	if err != nil || maximumBody < 1024 || maximumBody > 10*1024*1024 {
		return GitHubWebhook{}, errors.New("ORBIT_DEVOPS_GITHUB_WEBHOOK_MAX_BODY_BYTES must be between 1024 and 10485760")
	}
	endpoints := map[string]GitHubWebhookSecrets{}
	configured := strings.TrimSpace(os.Getenv("ORBIT_DEVOPS_GITHUB_WEBHOOK_ENDPOINTS"))
	if configured != "" {
		if err := json.Unmarshal([]byte(configured), &endpoints); err != nil {
			return GitHubWebhook{}, fmt.Errorf("parse ORBIT_DEVOPS_GITHUB_WEBHOOK_ENDPOINTS: %w", err)
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
	pool, err := loadDatabasePool(12)
	if err != nil {
		return ReleaseWorker{}, err
	}
	tracing, err := loadTracing()
	if err != nil {
		return ReleaseWorker{}, err
	}
	kubernetes, err := loadKubernetes()
	if err != nil {
		return ReleaseWorker{}, err
	}
	pollInterval, err := duration("ORBIT_DEVOPS_RELEASE_WORKER_POLL_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return ReleaseWorker{}, err
	}
	leaseDuration, err := duration("ORBIT_DEVOPS_RELEASE_WORKER_LEASE_DURATION", 10*time.Second)
	if err != nil {
		return ReleaseWorker{}, err
	}
	releaseOperationTimeout, err := duration("ORBIT_DEVOPS_RELEASE_OPERATION_TIMEOUT", 2*time.Minute)
	if err != nil {
		return ReleaseWorker{}, err
	}
	queue, err := loadReleaseQueue(releaseOperationTimeout)
	if err != nil {
		return ReleaseWorker{}, err
	}
	maximumAutomaticRetries, err := nonNegativeInteger(
		"ORBIT_DEVOPS_RELEASE_MAX_AUTOMATIC_RETRIES",
		2,
	)
	if err != nil {
		return ReleaseWorker{}, err
	}
	retryBaseDelay, err := duration("ORBIT_DEVOPS_RELEASE_RETRY_BASE_DELAY", time.Second)
	if err != nil {
		return ReleaseWorker{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return ReleaseWorker{}, fmt.Errorf("read hostname: %w", err)
	}
	return ReleaseWorker{
		DatabasePool:            pool,
		Tracing:                 tracing,
		Address:                 value("ORBIT_DEVOPS_RELEASE_WORKER_ADDRESS", "127.0.0.1:9091"),
		DatabaseURL:             value("ORBIT_DEVOPS_DATABASE_URL", defaultDatabaseURL),
		WorkerID:                value("ORBIT_DEVOPS_RELEASE_WORKER_ID", hostname),
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
	connection, err := loadKubernetesConnection()
	if err != nil {
		return BuildWorker{}, err
	}
	pool, err := loadDatabasePool(8)
	if err != nil {
		return BuildWorker{}, err
	}
	tracing, err := loadTracing()
	if err != nil {
		return BuildWorker{}, err
	}
	pollInterval, err := duration("ORBIT_DEVOPS_BUILD_WORKER_POLL_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return BuildWorker{}, err
	}
	leaseDuration, err := duration("ORBIT_DEVOPS_BUILD_WORKER_LEASE_DURATION", 15*time.Second)
	if err != nil {
		return BuildWorker{}, err
	}
	buildTimeout, err := duration("ORBIT_DEVOPS_BUILD_OPERATION_TIMEOUT", 20*time.Minute)
	if err != nil {
		return BuildWorker{}, err
	}
	queue, err := loadBuildQueue(buildTimeout)
	if err != nil {
		return BuildWorker{}, err
	}
	maximumRetries, err := nonNegativeInteger("ORBIT_DEVOPS_BUILD_MAX_AUTOMATIC_RETRIES", 2)
	if err != nil {
		return BuildWorker{}, err
	}
	retryBaseDelay, err := duration("ORBIT_DEVOPS_BUILD_RETRY_BASE_DELAY", 5*time.Second)
	if err != nil {
		return BuildWorker{}, err
	}
	jobTTL, err := duration("ORBIT_DEVOPS_BUILD_JOB_TTL", time.Hour)
	if err != nil {
		return BuildWorker{}, err
	}
	registryInsecure, err := boolean("ORBIT_DEVOPS_BUILD_REGISTRY_INSECURE", false)
	if err != nil {
		return BuildWorker{}, err
	}
	dockerHubMirrorInsecure, err := boolean("ORBIT_DEVOPS_BUILD_DOCKERHUB_MIRROR_INSECURE", false)
	if err != nil {
		return BuildWorker{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return BuildWorker{}, fmt.Errorf("read hostname: %w", err)
	}
	config := BuildWorker{
		KubernetesConnection: connection,
		DatabasePool:         pool, Tracing: tracing,
		Address: value("ORBIT_DEVOPS_BUILD_WORKER_ADDRESS", "127.0.0.1:9092"), DatabaseURL: value("ORBIT_DEVOPS_DATABASE_URL", defaultDatabaseURL),
		WorkerID: value("ORBIT_DEVOPS_BUILD_WORKER_ID", hostname), PollInterval: pollInterval, LeaseDuration: leaseDuration,
		BuildOperationTimeout: buildTimeout, MaximumAutomaticRetries: maximumRetries, RetryBaseDelay: retryBaseDelay,
		Namespace: value("ORBIT_DEVOPS_BUILD_NAMESPACE", "orbit-devops-s4-build"), FieldManager: value("ORBIT_DEVOPS_BUILD_FIELD_MANAGER", "orbit-devops-build-worker"),
		GitImage:           value("ORBIT_DEVOPS_BUILD_GIT_IMAGE", "alpine/git:v2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"),
		BuildkitImage:      value("ORBIT_DEVOPS_BUILDKIT_IMAGE", "moby/buildkit:v0.33.0-rootless@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef"),
		RegistrySecretName: os.Getenv("ORBIT_DEVOPS_BUILD_REGISTRY_SECRET"), RegistryInsecure: registryInsecure, JobTTL: jobTTL,
		DockerHubMirror: os.Getenv("ORBIT_DEVOPS_BUILD_DOCKERHUB_MIRROR"), DockerHubMirrorInsecure: dockerHubMirrorInsecure,
		CPU: value("ORBIT_DEVOPS_BUILD_CPU", "1"), Memory: value("ORBIT_DEVOPS_BUILD_MEMORY", "1Gi"), BuildQueue: queue,
	}
	for name, image := range map[string]string{"ORBIT_DEVOPS_BUILD_GIT_IMAGE": config.GitImage, "ORBIT_DEVOPS_BUILDKIT_IMAGE": config.BuildkitImage} {
		parts := strings.Split(image, "@sha256:")
		if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 64 {
			return BuildWorker{}, fmt.Errorf("%s must be pinned by sha256 digest", name)
		}
	}
	return config, nil
}

func loadBuildQueue(buildTimeout time.Duration) (ReleaseQueue, error) {
	config := ReleaseQueue{RedisAddress: value("ORBIT_DEVOPS_REDIS_ADDRESS", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("ORBIT_DEVOPS_REDIS_USERNAME"), RedisPassword: os.Getenv("ORBIT_DEVOPS_REDIS_PASSWORD"),
		Name: value("ORBIT_DEVOPS_BUILD_QUEUE_NAME", "orbit-devops-build")}
	var err error
	config.RedisDB, err = nonNegativeInteger("ORBIT_DEVOPS_REDIS_DB", 0)
	if err != nil {
		return ReleaseQueue{}, err
	}
	config.Concurrency, err = nonNegativeInteger("ORBIT_DEVOPS_BUILD_QUEUE_CONCURRENCY", 2)
	if err != nil || config.Concurrency < 1 || config.Concurrency > 100 {
		return ReleaseQueue{}, errors.New("ORBIT_DEVOPS_BUILD_QUEUE_CONCURRENCY must be between 1 and 100")
	}
	for _, item := range []struct {
		name     string
		fallback time.Duration
		target   *time.Duration
	}{
		{"ORBIT_DEVOPS_BUILD_QUEUE_REPAIR_INTERVAL", 5 * time.Second, &config.RepairInterval},
		{"ORBIT_DEVOPS_BUILD_QUEUE_CONSUMPTION_GRACE", 30 * time.Second, &config.ConsumptionGrace},
		{"ORBIT_DEVOPS_BUILD_QUEUE_TASK_TIMEOUT", buildTimeout + time.Minute, &config.TaskTimeout},
		{"ORBIT_DEVOPS_BUILD_QUEUE_SHUTDOWN_TIMEOUT", 30 * time.Second, &config.ShutdownTimeout},
	} {
		*item.target, err = duration(item.name, item.fallback)
		if err != nil {
			return ReleaseQueue{}, err
		}
	}
	if config.TaskTimeout < buildTimeout+time.Minute {
		return ReleaseQueue{}, errors.New("ORBIT_DEVOPS_BUILD_QUEUE_TASK_TIMEOUT must exceed build operation timeout by at least 1m")
	}
	return config, nil
}

func loadKubernetes() (Kubernetes, error) {
	connection, err := loadKubernetesConnection()
	if err != nil {
		return Kubernetes{}, err
	}
	pollInterval, err := duration("ORBIT_DEVOPS_KUBERNETES_POLL_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return Kubernetes{}, err
	}
	return Kubernetes{
		Connection:   connection,
		ClusterRef:   value("ORBIT_DEVOPS_CLUSTER_REF", "kind-orbit-devops-s1"),
		Namespace:    value("ORBIT_DEVOPS_NAMESPACE", "orbit-devops-s1"),
		FieldManager: value("ORBIT_DEVOPS_FIELD_MANAGER", "orbit-devops-worker"),
		PollInterval: pollInterval,
	}, nil
}

// loadKubernetesConnection 不探测网络；部署模式错误及缺少云端身份锚点在启动装配前被拒绝。
func loadKubernetesConnection() (kubeconnection.Config, error) {
	config := kubeconnection.Config{
		Mode:           value("ORBIT_DEVOPS_KUBERNETES_MODE", kubeconnection.LocalKind),
		KubeconfigPath: value("ORBIT_DEVOPS_KUBECONFIG", clientcmd.RecommendedHomeFile),
		Context:        value("ORBIT_DEVOPS_KUBERNETES_CONTEXT", "kind-orbit-devops-s1"),
		ClusterUID:     os.Getenv("ORBIT_DEVOPS_KUBERNETES_CLUSTER_UID"),
	}
	return config, config.Validate()
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
	if config.ClusterRef == "" ||
		config.Namespace == "" || config.FieldManager == "" {
		return errors.New("Kubernetes configuration is incomplete")
	}
	if err := config.Connection.Validate(); err != nil {
		return err
	}
	return nil
}
