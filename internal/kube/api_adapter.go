package kube

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/diagnostics"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

const apiClusterReadTimeout = 5 * time.Second

// APIAdapter 仅暴露 API 所需的读取与验证接口，不提供 Publish/Reconcile。
// 配置与安全错误阻止启动；短暂不可达时保留数据库服务，后续请求有界重试且重新核验身份。
type APIAdapter struct {
	base            *Adapter
	restConfig      *rest.Config
	expectedContext string
	gatewayClass    string
	gate            chan struct{}
	mu              sync.Mutex
	verifiedUntil   time.Time
	retryAfter      time.Time
	gateway         *GatewayAdapter
}

func NewAPIAdapter(ctx context.Context, path, expectedContext string, config Config, gatewayClass string) (*APIAdapter, error) {
	base, restConfig, err := newLocalAdapter(path, expectedContext, config)
	if err != nil {
		return nil, err
	}
	adapter := &APIAdapter{base: base, restConfig: restConfig, expectedContext: expectedContext,
		gatewayClass: gatewayClass, gate: make(chan struct{}, 1)}
	checkContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := adapter.verify(checkContext); err != nil {
		if ctx.Err() != nil || !clusterTemporarilyUnavailable(err) {
			return nil, err
		}
	}
	return adapter, nil
}

// verify 合并并发核验，失败后短暂退避；不使用后台无限重试，也不保存启动时的不可达结果为永久状态。
func (a *APIAdapter) verify(ctx context.Context) error {
	select {
	case a.gate <- struct{}{}:
		defer func() { <-a.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	a.mu.Lock()
	ready, waiting := time.Now().Before(a.verifiedUntil), time.Now().Before(a.retryAfter)
	a.mu.Unlock()
	if ready {
		return nil
	}
	if waiting {
		return diagnostics.ErrKubernetesUnavailable
	}
	if err := verifyKindIdentity(ctx, a.base.client, a.expectedContext, a.base.config.Namespace); err != nil {
		a.invalidate()
		return err
	}
	// DynamicClient 仍只在在线安全核验成功后获得 REST 配置；Worker 不使用此降级装配。
	if a.gateway == nil && a.gatewayClass != "" {
		a.base.restConfig = a.restConfig
		gateway, err := NewGatewayAdapter(a.base, a.gatewayClass)
		if err != nil {
			return err
		}
		a.gateway = gateway
	}
	a.mu.Lock()
	a.verifiedUntil, a.retryAfter = time.Now().Add(apiClusterReadTimeout), time.Time{}
	a.mu.Unlock()
	return nil
}

func (a *APIAdapter) invalidate() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verifiedUntil, a.retryAfter = time.Time{}, time.Now().Add(time.Second)
}

func clusterTemporarilyUnavailable(err error) bool {
	var networkError *net.OpError
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsInternalError(err)
}

func (a *APIAdapter) ObserveTarget(ctx context.Context, query diagnostics.TargetRuntimeQuery) diagnostics.RuntimeObservation {
	readContext, cancel := context.WithTimeout(ctx, apiClusterReadTimeout)
	defer cancel()
	if err := a.verify(readContext); err != nil {
		return diagnostics.UnavailableSource{}.ObserveTarget(ctx, query)
	}
	result := a.base.ObserveTarget(readContext, query)
	if result.Workload.Metadata.Status == diagnostics.ObservationUnavailable {
		a.invalidate()
	}
	return result
}

func (a *APIAdapter) ReadReleaseLogs(ctx context.Context, query diagnostics.ReleaseRuntimeLogQuery) (diagnostics.RuntimeLogResult, error) {
	readContext, cancel := context.WithTimeout(ctx, apiClusterReadTimeout)
	defer cancel()
	if err := a.verify(readContext); err != nil {
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrKubernetesUnavailable
	}
	result, err := a.base.ReadReleaseLogs(readContext, query)
	if errors.Is(err, diagnostics.ErrKubernetesUnavailable) {
		a.invalidate()
	}
	return result, err
}

func (a *APIAdapter) VerifyTLSSecret(ctx context.Context, namespace, name, hostname string) error {
	readContext, cancel := context.WithTimeout(ctx, apiClusterReadTimeout)
	defer cancel()
	if err := a.verify(readContext); err != nil {
		return identity.ErrUnavailable
	}
	err := a.base.VerifyTLSSecret(readContext, namespace, name, hostname)
	if errors.Is(err, identity.ErrUnavailable) {
		a.invalidate()
	}
	return err
}

func (a *APIAdapter) ObserveHost(ctx context.Context, snapshot access.Snapshot, host access.HostSpec, routes []access.RouteSpec) (access.ControllerObservation, error) {
	readContext, cancel := context.WithTimeout(ctx, apiClusterReadTimeout)
	defer cancel()
	if err := a.verify(readContext); err != nil || a.gatewayClass == "" {
		return access.ControllerObservation{}, diagnostics.ErrKubernetesUnavailable
	}
	result, err := a.gateway.ObserveHost(readContext, snapshot, host, routes)
	if err != nil {
		a.invalidate()
	}
	return result, err
}

func (a *APIAdapter) InspectRecovery(ctx context.Context, request releaseworker.PublishRequest) (releaseworker.RecoveryObservation, error) {
	readContext, cancel := context.WithTimeout(ctx, apiClusterReadTimeout)
	defer cancel()
	if err := a.verify(readContext); err != nil {
		return releaseworker.RecoveryObservation{}, diagnostics.ErrKubernetesUnavailable
	}
	result, err := a.base.InspectRecovery(readContext, request)
	if err != nil {
		a.invalidate()
	}
	return result, err
}

func (a *APIAdapter) ObserveRecovery(ctx context.Context, request releaseworker.PublishRequest) error {
	readContext, cancel := context.WithTimeout(ctx, apiClusterReadTimeout)
	defer cancel()
	if err := a.verify(readContext); err != nil {
		return diagnostics.ErrKubernetesUnavailable
	}
	err := a.base.ObserveRecovery(readContext, request)
	if err != nil {
		a.invalidate()
	}
	return err
}

var _ diagnostics.RuntimeSource = (*APIAdapter)(nil)
var _ access.ControllerObserver = (*APIAdapter)(nil)
var _ access.SecretVerifier = (*APIAdapter)(nil)
var _ releaseworker.RecoveryPublisher = (*APIAdapter)(nil)
