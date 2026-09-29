package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const (
	AccessHostIDLabel           = "orbit-devops.dev/access-host-id"
	AccessRouteIDLabel          = "orbit-devops.dev/access-route-id"
	AccessRevisionAnnotation    = "orbit-devops.dev/access-revision"
	AccessCertificateAnnotation = "orbit-devops.dev/access-certificate"
)

var (
	gatewayResource     = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	httpRouteResource   = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	certificateResource = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	ErrAccessOwnership  = errors.New("access Kubernetes resource ownership conflict")
	ErrAccessBoundary   = errors.New("access Kubernetes boundary violation")
	ErrAccessPending    = errors.New("access Kubernetes controller has not observed the updated Gateway")
)

type GatewayAdapter struct {
	base      *Adapter
	dynamic   dynamic.Interface
	className string
}

// NewGatewayAdapter 只从已核验的本地 Kind Adapter 构建入口写入端。
func NewGatewayAdapter(base *Adapter, gatewayClassName string) (*GatewayAdapter, error) {
	if base == nil || gatewayClassName == "" {
		return nil, errors.New("gateway adapter configuration is incomplete")
	}
	client, err := base.DynamicClient()
	if err != nil {
		return nil, err
	}
	return &GatewayAdapter{base: base, dynamic: client, className: gatewayClassName}, nil
}

func GatewayName(projectID uuid.UUID) string        { return "orbit-gw-" + projectID.String() }
func AccessRouteName(routeID uuid.UUID) string      { return "orbit-route-" + routeID.String() }
func AccessRedirectName(hostID uuid.UUID) string    { return "orbit-redirect-" + hostID.String() }
func AccessCertificateName(hostID uuid.UUID) string { return "orbit-cert-" + hostID.String() }
func AccessTLSSecretName(hostID uuid.UUID) string   { return "orbit-tls-" + hostID.String() }
func accessListenerName(hostID uuid.UUID) string    { return "https-" + hostID.String() }

func accessLabels(projectID, hostID, routeID uuid.UUID) map[string]string {
	result := map[string]string{ManagedByLabel: ManagedByValue, ProjectIDLabel: projectID.String()}
	if hostID != uuid.Nil {
		result[AccessHostIDLabel] = hostID.String()
	}
	if routeID != uuid.Nil {
		result[AccessRouteIDLabel] = routeID.String()
	}
	return result
}

func hasAccessOwnership(existing, expected map[string]string) bool {
	for _, key := range []string{ManagedByLabel, ProjectIDLabel, AccessHostIDLabel, AccessRouteIDLabel} {
		if existing[key] != expected[key] {
			return false
		}
	}
	return true
}

func accessMetadata(name, namespace string, labels map[string]string, revision int64) map[string]any {
	return map[string]any{"name": name, "namespace": namespace, "labels": accessStringMap(labels),
		"annotations": map[string]any{AccessRevisionAnnotation: fmt.Sprint(revision)}}
}

func accessStringMap(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func gatewayObject(snapshot access.Snapshot, className string, hosts []access.HostSpec) *unstructured.Unstructured {
	listeners := []any{map[string]any{"name": "http", "port": int64(80), "protocol": "HTTP",
		"allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Same"}}}}
	for _, spec := range hosts {
		if spec.Host.TLSMode == "http_only" {
			continue
		}
		secret := spec.SecretName
		if spec.Host.TLSMode == "managed" {
			secret = AccessTLSSecretName(spec.Host.ID)
		}
		listeners = append(listeners, map[string]any{"name": accessListenerName(spec.Host.ID), "port": int64(443),
			"protocol": "HTTPS", "hostname": spec.Host.Hostname,
			"allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Same"}},
			"tls": map[string]any{"mode": "Terminate", "certificateRefs": []any{
				map[string]any{"group": "", "kind": "Secret", "name": secret}}}})
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1",
		"kind": "Gateway", "metadata": accessMetadata(GatewayName(snapshot.ProjectID), snapshot.Namespace,
			accessLabels(snapshot.ProjectID, uuid.Nil, uuid.Nil), snapshot.Revision),
		"spec": map[string]any{"gatewayClassName": className, "listeners": listeners}}}
}

func certificateObject(snapshot access.Snapshot, host access.HostSpec) *unstructured.Unstructured {
	owner := accessLabels(snapshot.ProjectID, host.Host.ID, uuid.Nil)
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cert-manager.io/v1",
		"kind": "Certificate", "metadata": accessMetadata(AccessCertificateName(host.Host.ID), snapshot.Namespace,
			owner, snapshot.Revision),
		"spec": map[string]any{"secretName": AccessTLSSecretName(host.Host.ID),
			"dnsNames": []any{host.Host.Hostname},
			"issuerRef": map[string]any{"group": "cert-manager.io", "kind": host.Issuer.Kind,
				"name": host.Issuer.Name},
			"secretTemplate": map[string]any{"labels": accessStringMap(owner),
				"annotations": map[string]any{AccessCertificateAnnotation: AccessCertificateName(host.Host.ID)}}}}}
}

func routeObject(snapshot access.Snapshot, host access.HostSpec, route access.RouteSpec) *unstructured.Unstructured {
	section := "http"
	if host.Host.TLSMode != "http_only" {
		section = accessListenerName(host.Host.ID)
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1",
		"kind": "HTTPRoute", "metadata": accessMetadata(AccessRouteName(route.Route.ID), snapshot.Namespace,
			accessLabels(snapshot.ProjectID, host.Host.ID, route.Route.ID), snapshot.Revision),
		"spec": map[string]any{"parentRefs": []any{map[string]any{"name": GatewayName(snapshot.ProjectID), "sectionName": section}},
			"hostnames": []any{host.Host.Hostname}, "rules": []any{map[string]any{
				"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": route.Route.PathPrefix}}},
				"backendRefs": []any{map[string]any{"name": ResourceName(route.Route.DeploymentTargetID), "port": int64(route.ServicePort)}}}}}}}
}

func redirectObject(snapshot access.Snapshot, host access.HostSpec) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1",
		"kind": "HTTPRoute", "metadata": accessMetadata(AccessRedirectName(host.Host.ID), snapshot.Namespace,
			accessLabels(snapshot.ProjectID, host.Host.ID, uuid.Nil), snapshot.Revision),
		"spec": map[string]any{"parentRefs": []any{map[string]any{"name": GatewayName(snapshot.ProjectID), "sectionName": "http"}},
			"hostnames": []any{host.Host.Hostname}, "rules": []any{map[string]any{
				"matches": []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}},
				"filters": []any{map[string]any{"type": "RequestRedirect", "requestRedirect": map[string]any{
					"scheme": "https", "statusCode": int64(301)}}}}}}}}
}

// Reconcile 始终渲染整个 Project 的最新集合，并在每次写入前检查数据库租约。
func (a *GatewayAdapter) Reconcile(ctx context.Context, snapshot access.Snapshot, guard func(context.Context) error) error {
	if snapshot.ClusterRef != a.base.config.ClusterRef || snapshot.Namespace != a.base.config.Namespace ||
		snapshot.GatewayClassName != a.className || snapshot.ProjectID == uuid.Nil || guard == nil {
		return ErrAccessBoundary
	}
	activeHosts := make([]access.HostSpec, 0, len(snapshot.Hosts))
	hostByID := make(map[uuid.UUID]access.HostSpec, len(snapshot.Hosts))
	certificates := make(map[string]*unstructured.Unstructured)
	routes := make(map[string]*unstructured.Unstructured)
	managedSecrets := make(map[string]struct{})
	for _, host := range snapshot.Hosts {
		if host.Host.Lifecycle != "active" {
			continue
		}
		if host.Host.TLSMode == "existing_secret" && (!host.BindingActive || host.SecretName == "") {
			continue
		}
		if host.Host.TLSMode == "managed" {
			if host.Issuer.Name == "" || host.Issuer.Kind != "Issuer" && host.Issuer.Kind != "ClusterIssuer" {
				return ErrAccessBoundary
			}
			if err := a.checkManagedSecretOwnership(ctx, snapshot.ProjectID, host.Host.ID); err != nil {
				return err
			}
			certificates[AccessCertificateName(host.Host.ID)] = certificateObject(snapshot, host)
			managedSecrets[AccessTLSSecretName(host.Host.ID)] = struct{}{}
		}
		activeHosts = append(activeHosts, host)
		hostByID[host.Host.ID] = host
		if host.Host.TLSMode != "http_only" {
			routes[AccessRedirectName(host.Host.ID)] = redirectObject(snapshot, host)
		}
	}
	for _, route := range snapshot.Routes {
		if route.Route.Lifecycle != "active" {
			continue
		}
		host, ok := hostByID[route.Route.HostID]
		if !ok {
			continue
		}
		service, err := a.checkBackendOwnership(ctx, snapshot.ProjectID, route.Route.DeploymentTargetID)
		if err != nil {
			return err
		}
		route.ServicePort = routeBackendPort(service)
		routes[AccessRouteName(route.Route.ID)] = routeObject(snapshot, host, route)
	}
	// 旧 Route 先退出匹配；之后才修改 listener 或证书引用。
	if err := a.deleteStaleDynamic(ctx, httpRouteResource, snapshot.ProjectID, routes, guard); err != nil {
		return err
	}
	for _, object := range certificates {
		if err := a.applyDynamic(ctx, certificateResource, object, guard); err != nil {
			return err
		}
	}
	if len(activeHosts) > 0 {
		if err := a.applyDynamic(ctx, gatewayResource, gatewayObject(snapshot, a.className, activeHosts), guard); err != nil {
			return err
		}
		for _, object := range routes {
			if err := a.applyDynamic(ctx, httpRouteResource, object, guard); err != nil {
				return err
			}
		}
	} else {
		if err := a.deleteDynamic(ctx, gatewayResource, GatewayName(snapshot.ProjectID),
			accessLabels(snapshot.ProjectID, uuid.Nil, uuid.Nil), guard); err != nil {
			return err
		}
	}
	// 新 Gateway spec 不再引用旧证书后，才删除 Orbit 自有 Certificate 和托管 Secret。
	needsCleanup, err := a.needsManagedCleanup(ctx, snapshot.ProjectID, certificates, managedSecrets)
	if err != nil {
		return err
	}
	if needsCleanup {
		if err := a.confirmGatewayDetached(ctx, snapshot.ProjectID, len(activeHosts) > 0); err != nil {
			return err
		}
	}
	if err := a.deleteStaleDynamic(ctx, certificateResource, snapshot.ProjectID, certificates, guard); err != nil {
		return err
	}
	if err := a.deleteStaleManagedSecrets(ctx, snapshot.ProjectID, managedSecrets, guard); err != nil {
		return err
	}
	return nil
}

func (a *GatewayAdapter) checkManagedSecretOwnership(ctx context.Context, projectID, hostID uuid.UUID) error {
	secret, err := a.base.client.CoreV1().Secrets(a.base.config.Namespace).Get(ctx, AccessTLSSecretName(hostID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedManagedTLSSecret(*secret, projectID) {
		return ErrAccessOwnership
	}
	return nil
}

// Service 可以尚未由 Release 创建；若稳定名称已存在，则必须确属该 Target。
func (a *GatewayAdapter) checkBackendOwnership(ctx context.Context, projectID, targetID uuid.UUID) (*corev1.Service, error) {
	service, err := a.base.client.CoreV1().Services(a.base.config.Namespace).Get(ctx, ResourceName(targetID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	labels := service.GetLabels()
	if labels[ManagedByLabel] != ManagedByValue || labels[ProjectIDLabel] != projectID.String() || labels[TargetIDLabel] != targetID.String() {
		return nil, ErrAccessOwnership
	}
	return service, nil
}

// routeBackendPort 只根据已存在的 Service 选端口；Target 期望配置不驱动入口提前切换。
func routeBackendPort(service *corev1.Service) int32 {
	if service == nil {
		return ServiceHTTPPort
	}
	for _, port := range service.Spec.Ports {
		if port.Port == ServiceHTTPPort && port.Protocol == corev1.ProtocolTCP {
			return ServiceHTTPPort
		}
	}
	for _, port := range service.Spec.Ports {
		if port.Port > 0 && port.Protocol == corev1.ProtocolTCP {
			return port.Port
		}
	}
	return ServiceHTTPPort
}

// 删除托管证书前确认新 Gateway 已被控制器观察，防止切换 TLS 时过早撤掉仍被使用的 Secret。
func (a *GatewayAdapter) confirmGatewayDetached(ctx context.Context, projectID uuid.UUID, gatewayExpected bool) error {
	gateway, err := a.dynamic.Resource(gatewayResource).Namespace(a.base.config.Namespace).Get(ctx, GatewayName(projectID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if gatewayExpected {
			return ErrAccessPending
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !hasAccessOwnership(gateway.GetLabels(), accessLabels(projectID, uuid.Nil, uuid.Nil)) {
		return ErrAccessOwnership
	}
	if !gatewayExpected || readyConditions(gateway, "Programmed") != "ready" {
		return ErrAccessPending
	}
	return nil
}

func (a *GatewayAdapter) needsManagedCleanup(ctx context.Context, projectID uuid.UUID,
	certificates map[string]*unstructured.Unstructured, secrets map[string]struct{}) (bool, error) {
	selector := labels.SelectorFromSet(map[string]string{ManagedByLabel: ManagedByValue, ProjectIDLabel: projectID.String()}).String()
	listed, err := a.dynamic.Resource(certificateResource).Namespace(a.base.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	for _, object := range listed.Items {
		if _, found := certificates[object.GetName()]; !found {
			return true, nil
		}
	}
	listedSecrets, err := a.base.client.CoreV1().Secrets(a.base.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	for _, secret := range listedSecrets.Items {
		if _, found := secrets[secret.Name]; found {
			continue
		}
		if ownedManagedTLSSecret(secret, projectID) {
			return true, nil
		}
	}
	return false, nil
}

func (a *GatewayAdapter) applyDynamic(ctx context.Context, resource schema.GroupVersionResource,
	object *unstructured.Unstructured, guard func(context.Context) error) error {
	client := a.dynamic.Resource(resource).Namespace(a.base.config.Namespace)
	existing, err := client.Get(ctx, object.GetName(), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && !hasAccessOwnership(existing.GetLabels(), object.GetLabels()) {
		return ErrAccessOwnership
	}
	if err := guard(ctx); err != nil {
		return err
	}
	payload, err := json.Marshal(object.Object)
	if err != nil {
		return err
	}
	_, err = client.Patch(ctx, object.GetName(), types.ApplyPatchType, payload,
		metav1.PatchOptions{FieldManager: a.base.config.FieldManager})
	return err
}

func (a *GatewayAdapter) deleteDynamic(ctx context.Context, resource schema.GroupVersionResource,
	name string, owner map[string]string, guard func(context.Context) error) error {
	client := a.dynamic.Resource(resource).Namespace(a.base.config.Namespace)
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !hasAccessOwnership(existing.GetLabels(), owner) {
		return ErrAccessOwnership
	}
	if err := guard(ctx); err != nil {
		return err
	}
	err = client.Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (a *GatewayAdapter) deleteStaleDynamic(ctx context.Context, resource schema.GroupVersionResource,
	projectID uuid.UUID, desired map[string]*unstructured.Unstructured, guard func(context.Context) error) error {
	selector := labels.SelectorFromSet(map[string]string{ManagedByLabel: ManagedByValue, ProjectIDLabel: projectID.String()}).String()
	list, err := a.dynamic.Resource(resource).Namespace(a.base.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for _, existing := range list.Items {
		if _, ok := desired[existing.GetName()]; ok {
			continue
		}
		if !ownedAccessName(resource, existing) {
			return ErrAccessOwnership
		}
		if err := a.deleteDynamic(ctx, resource, existing.GetName(), existing.GetLabels(), guard); err != nil {
			return err
		}
	}
	return nil
}

func ownedAccessName(resource schema.GroupVersionResource, object unstructured.Unstructured) bool {
	hostID := object.GetLabels()[AccessHostIDLabel]
	routeID := object.GetLabels()[AccessRouteIDLabel]
	switch resource.Resource {
	case "httproutes":
		if _, err := uuid.Parse(hostID); err != nil {
			return false
		}
		if routeID != "" {
			id, err := uuid.Parse(routeID)
			return err == nil && object.GetName() == AccessRouteName(id)
		}
		id, err := uuid.Parse(hostID)
		return err == nil && object.GetName() == AccessRedirectName(id)
	case "certificates":
		id, err := uuid.Parse(hostID)
		return err == nil && object.GetName() == AccessCertificateName(id)
	default:
		return false
	}
}

func (a *GatewayAdapter) deleteStaleManagedSecrets(ctx context.Context, projectID uuid.UUID,
	desired map[string]struct{}, guard func(context.Context) error) error {
	selector := labels.SelectorFromSet(map[string]string{ManagedByLabel: ManagedByValue, ProjectIDLabel: projectID.String()}).String()
	list, err := a.base.client.CoreV1().Secrets(a.base.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for _, secret := range list.Items {
		if _, ok := desired[secret.Name]; ok {
			continue
		}
		if !ownedManagedTLSSecret(secret, projectID) {
			continue
		}
		if err := guard(ctx); err != nil {
			return err
		}
		if err := a.base.client.CoreV1().Secrets(a.base.config.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func ownedManagedTLSSecret(secret corev1.Secret, projectID uuid.UUID) bool {
	id, err := uuid.Parse(secret.Labels[AccessHostIDLabel])
	return err == nil && secret.Name == AccessTLSSecretName(id) && secret.Type == corev1.SecretTypeTLS &&
		secret.Labels[ManagedByLabel] == ManagedByValue && secret.Labels[ProjectIDLabel] == projectID.String() &&
		secret.Annotations[AccessCertificateAnnotation] == AccessCertificateName(id)
}

// AccessErrorCode 只将可公开的有限错误码写入同步表，不保存集群错误正文。
func AccessErrorCode(err error) string {
	if errors.Is(err, ErrAccessOwnership) {
		return "access_ownership_conflict"
	}
	if errors.Is(err, ErrAccessBoundary) {
		return "access_boundary_violation"
	}
	if errors.Is(err, ErrAccessPending) {
		return "access_controller_pending"
	}
	if apierrors.IsNotFound(err) {
		return "access_dependency_not_found"
	}
	if apierrors.IsForbidden(err) {
		return "access_kubernetes_forbidden"
	}
	if strings.Contains(err.Error(), "the server could not find the requested resource") {
		return "access_crd_unavailable"
	}
	return "access_kubernetes_unavailable"
}
