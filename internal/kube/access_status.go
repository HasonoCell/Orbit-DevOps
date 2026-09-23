package kube

import (
	"context"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ObserveHost 点时读取控制器报告，不把 Apply 成功推断成控制器 Ready。
func (a *GatewayAdapter) ObserveHost(ctx context.Context, snapshot access.Snapshot, host access.HostSpec, routes []access.RouteSpec) (access.ControllerObservation, error) {
	if snapshot.ClusterRef != a.base.config.ClusterRef || snapshot.Namespace != a.base.config.Namespace || snapshot.ProjectID == uuid.Nil || host.Host.ProjectID != snapshot.ProjectID {
		return access.ControllerObservation{}, ErrAccessBoundary
	}
	result := access.ControllerObservation{GatewayState: "unknown", ListenerState: "unknown",
		CertificateState: "not_applicable", SecretState: "not_applicable", Routes: make([]access.RouteObservation, 0, len(routes)),
		Addresses: []string{}, ObservedAt: time.Now().UTC()}
	gateway, err := a.observeResource(ctx, gatewayResource, GatewayName(snapshot.ProjectID), accessLabels(snapshot.ProjectID, uuid.Nil, uuid.Nil))
	if err != nil {
		return result, err
	}
	if gateway != nil {
		result.GatewayState = readyConditions(gateway, "Accepted", "Programmed")
		listenerName := "http"
		if host.Host.TLSMode != "http_only" {
			listenerName = accessListenerName(host.Host.ID)
		}
		listeners, _, _ := unstructured.NestedSlice(gateway.Object, "status", "listeners")
		for _, item := range listeners {
			listener, ok := item.(map[string]any)
			if ok && listener["name"] == listenerName {
				conditionObject := &unstructured.Unstructured{Object: listener}
				conditionObject.SetGeneration(gateway.GetGeneration())
				result.ListenerState = readyConditions(conditionObject, "Accepted", "Programmed", "ResolvedRefs")
			}
		}
		addresses, _, _ := unstructured.NestedSlice(gateway.Object, "status", "addresses")
		for _, item := range addresses {
			address, ok := item.(map[string]any)
			if ok {
				if value, ok := address["value"].(string); ok && value != "" {
					result.Addresses = append(result.Addresses, value)
				}
			}
		}
	} else {
		result.GatewayState, result.ListenerState = "not_ready", "not_ready"
	}
	for _, route := range routes {
		observation := access.RouteObservation{RouteID: route.Route.ID, Accepted: "unknown", ResolvedRefs: "unknown"}
		object, readErr := a.observeResource(ctx, httpRouteResource, AccessRouteName(route.Route.ID),
			accessLabels(snapshot.ProjectID, host.Host.ID, route.Route.ID))
		if readErr != nil {
			return result, readErr
		}
		if object == nil {
			observation.Accepted, observation.ResolvedRefs = "not_ready", "not_ready"
		} else {
			parents, _, _ := unstructured.NestedSlice(object.Object, "status", "parents")
			for _, item := range parents {
				parent, ok := item.(map[string]any)
				if !ok {
					continue
				}
				ref, ok := parent["parentRef"].(map[string]any)
				if !ok || ref["name"] != GatewayName(snapshot.ProjectID) {
					continue
				}
				conditions := &unstructured.Unstructured{Object: parent}
				conditions.SetGeneration(object.GetGeneration())
				observation.Accepted = readyConditions(conditions, "Accepted")
				observation.ResolvedRefs = readyConditions(conditions, "ResolvedRefs")
			}
		}
		result.Routes = append(result.Routes, observation)
	}
	if host.Host.TLSMode == "http_only" {
		return result, nil
	}
	secretName := host.SecretName
	if host.Host.TLSMode == "managed" {
		secretName = AccessTLSSecretName(host.Host.ID)
		certificate, readErr := a.observeResource(ctx, certificateResource, AccessCertificateName(host.Host.ID),
			accessLabels(snapshot.ProjectID, host.Host.ID, uuid.Nil))
		if readErr != nil {
			return result, readErr
		}
		result.CertificateState = "not_ready"
		if certificate != nil {
			result.CertificateState = readyConditions(certificate, "Ready")
			if value, found, _ := unstructured.NestedString(certificate.Object, "status", "notAfter"); found {
				if notAfter, parseErr := time.Parse(time.RFC3339, value); parseErr == nil {
					result.CertificateNotAfter = &notAfter
				}
			}
		}
	}
	if secretName == "" || host.Host.TLSMode == "existing_secret" && !host.BindingActive {
		result.SecretState = "not_ready"
		return result, nil
	}
	secret, readErr := a.base.client.CoreV1().Secrets(snapshot.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(readErr) {
		result.SecretState = "not_ready"
		return result, nil
	}
	if readErr != nil {
		return result, readErr
	}
	result.SecretState = "not_ready"
	if secret.Type == corev1.SecretTypeTLS {
		result.SecretState = "ready"
	}
	return result, nil
}

func (a *GatewayAdapter) observeResource(ctx context.Context, resource schema.GroupVersionResource, name string, owner map[string]string) (*unstructured.Unstructured, error) {
	object, err := a.dynamic.Resource(resource).Namespace(a.base.config.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !hasOwnership(object.GetLabels(), owner) {
		return nil, ErrAccessOwnership
	}
	return object, nil
}

func readyConditions(object *unstructured.Unstructured, required ...string) string {
	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err == nil && len(conditions) == 0 {
		conditions, _, err = unstructured.NestedSlice(object.Object, "conditions")
	}
	if err != nil || len(conditions) == 0 {
		return "unknown"
	}
	seen := make(map[string]string, len(conditions))
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := condition["type"].(string)
		value, _ := condition["status"].(string)
		if generation := object.GetGeneration(); generation > 0 {
			observed, ok := condition["observedGeneration"].(int64)
			if !ok || observed < generation {
				continue
			}
		}
		seen[kind] = value
	}
	for _, kind := range required {
		if seen[kind] == "False" {
			return "not_ready"
		}
	}
	for _, kind := range required {
		if seen[kind] != "True" {
			return "unknown"
		}
	}
	return "ready"
}
