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
	Address          string
	DatabaseURL      string
	WorkerID         string
	PollInterval     time.Duration
	LeaseDuration    time.Duration
	OperationTimeout time.Duration
	Kubernetes       Kubernetes
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
		Address:       value("ORBITOPS_API_ADDRESS", ":8080"),
		DatabaseURL:   value("ORBITOPS_DATABASE_URL", defaultDatabaseURL),
		ActorID:       value("ORBITOPS_ACTOR_ID", "local-developer"),
		MigrateOnBoot: migrateOnBoot,
		Kubernetes:    kubernetes,
	}, nil
}

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
	hostname, err := os.Hostname()
	if err != nil {
		return Worker{}, fmt.Errorf("read hostname: %w", err)
	}
	return Worker{
		Address:          value("ORBITOPS_WORKER_ADDRESS", ":9091"),
		DatabaseURL:      value("ORBITOPS_DATABASE_URL", defaultDatabaseURL),
		WorkerID:         value("ORBITOPS_WORKER_ID", hostname),
		PollInterval:     pollInterval,
		LeaseDuration:    leaseDuration,
		OperationTimeout: operationTimeout,
		Kubernetes:       kubernetes,
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

func ValidateKubernetes(config Kubernetes) error {
	if config.KubeconfigPath == "" || config.Context == "" || config.ClusterRef == "" ||
		config.Namespace == "" || config.FieldManager == "" {
		return errors.New("Kubernetes configuration is incomplete")
	}
	return nil
}
