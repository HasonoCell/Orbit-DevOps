package kube

import (
	"context"
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGatewayRendersDistinctTLSListenersAndRoutes(t *testing.T) {
	projectID, hostID, targetID, routeID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	host := access.HostSpec{Host: access.Host{ID: hostID, ProjectID: projectID, Hostname: "shop.example.com", TLSMode: "managed", Lifecycle: "active"},
		Issuer: access.IssuerPolicy{Kind: "ClusterIssuer", Name: "local-ca"}}
	route := access.RouteSpec{Route: access.Route{ID: routeID, HostID: hostID, DeploymentTargetID: targetID, PathPrefix: "/api", Lifecycle: "active"}, ServicePort: 8080}
	snapshot := access.Snapshot{ProjectID: projectID, Namespace: "orbit-test", Revision: 4}
	gateway := gatewayObject(snapshot, "local-gateway", []access.HostSpec{host})
	listeners, found, err := unstructured.NestedSlice(gateway.Object, "spec", "listeners")
	if err != nil || !found || len(listeners) != 2 {
		t.Fatalf("unexpected listeners: %v %v %v", listeners, found, err)
	}
	https := listeners[1].(map[string]any)
	if https["hostname"] != "shop.example.com" || https["protocol"] != "HTTPS" {
		t.Fatalf("unexpected HTTPS listener: %v", https)
	}
	refs := https["tls"].(map[string]any)["certificateRefs"].([]any)
	if refs[0].(map[string]any)["name"] != AccessTLSSecretName(hostID) {
		t.Fatalf("unexpected certificate ref: %v", refs)
	}
	backend := routeObject(snapshot, host, route)
	parents, _, _ := unstructured.NestedSlice(backend.Object, "spec", "parentRefs")
	if parents[0].(map[string]any)["sectionName"] != accessListenerName(hostID) {
		t.Fatalf("backend not bound to HTTPS listener: %v", parents)
	}
	rules, _, _ := unstructured.NestedSlice(backend.Object, "spec", "rules")
	backends := rules[0].(map[string]any)["backendRefs"].([]any)
	if backends[0].(map[string]any)["name"] != ResourceName(targetID) || backends[0].(map[string]any)["port"] != int64(8080) {
		t.Fatalf("wrong target service: %v", backends)
	}
	redirect := redirectObject(snapshot, host)
	parents, _, _ = unstructured.NestedSlice(redirect.Object, "spec", "parentRefs")
	if parents[0].(map[string]any)["sectionName"] != "http" {
		t.Fatalf("redirect not bound to HTTP: %v", parents)
	}
	redirectRules, _, _ := unstructured.NestedSlice(redirect.Object, "spec", "rules")
	filters := redirectRules[0].(map[string]any)["filters"].([]any)
	if filters[0].(map[string]any)["type"] != "RequestRedirect" {
		t.Fatalf("missing redirect: %v", filters)
	}
	certificate := certificateObject(snapshot, host)
	secretName, _, _ := unstructured.NestedString(certificate.Object, "spec", "secretName")
	if secretName != AccessTLSSecretName(hostID) {
		t.Fatalf("wrong managed secret: %s", secretName)
	}
	marker, _, _ := unstructured.NestedString(certificate.Object, "spec", "secretTemplate", "annotations", AccessCertificateAnnotation)
	if marker != AccessCertificateName(hostID) {
		t.Fatalf("managed Secret marker missing: %s", marker)
	}
}

func TestAccessOwnershipRejectsUnrelatedObjects(t *testing.T) {
	projectID, hostID := uuid.New(), uuid.New()
	object := *redirectObject(access.Snapshot{ProjectID: projectID, Namespace: "orbit-test"},
		access.HostSpec{Host: access.Host{ID: hostID, Hostname: "shop.example.com", TLSMode: "managed"}})
	if !ownedAccessName(httpRouteResource, object) {
		t.Fatal("expected canonical redirect ownership")
	}
	object.SetName("other-team-route")
	if ownedAccessName(httpRouteResource, object) {
		t.Fatal("must not delete other object with copied labels")
	}
	if AccessErrorCode(ErrAccessOwnership) != "access_ownership_conflict" || !errors.Is(ErrAccessBoundary, ErrAccessBoundary) {
		t.Fatal("missing bounded ownership errors")
	}
	if AccessErrorCode(ErrAccessPending) != "access_controller_pending" {
		t.Fatal("pending controller must remain retryable")
	}
	owner := accessLabels(projectID, hostID, uuid.New())
	wrongHost := accessLabels(projectID, uuid.New(), uuid.New())
	if hasAccessOwnership(wrongHost, owner) {
		t.Fatal("Host identity must be checked")
	}
}

func TestGatewayRejectsForeignTargetService(t *testing.T) {
	projectID, targetID := uuid.New(), uuid.New()
	client := fake.NewSimpleClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: ResourceName(targetID), Namespace: "orbit-test", Labels: map[string]string{
			ManagedByLabel: ManagedByValue, ProjectIDLabel: uuid.New().String(), TargetIDLabel: targetID.String(),
		},
	}})
	base, err := New(client, Config{ClusterRef: "kind-orbit", Namespace: "orbit-test", FieldManager: "orbit-access"})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &GatewayAdapter{base: base}
	_, err = gateway.checkBackendOwnership(context.Background(), projectID, targetID)
	if !errors.Is(err, ErrAccessOwnership) {
		t.Fatal("foreign Service with the stable name must not receive traffic")
	}
}

func TestManagedTLSSecretCleanupRequiresDedicatedMarker(t *testing.T) {
	hostID, projectID := uuid.New(), uuid.New()
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: AccessTLSSecretName(hostID),
		Labels: map[string]string{AccessHostIDLabel: hostID.String(), ManagedByLabel: ManagedByValue, ProjectIDLabel: projectID.String()}}, Type: corev1.SecretTypeTLS}
	if ownedManagedTLSSecret(secret, projectID) {
		t.Fatal("labels alone must not authorize Secret deletion")
	}
	secret.Annotations = map[string]string{AccessCertificateAnnotation: AccessCertificateName(hostID)}
	if !ownedManagedTLSSecret(secret, projectID) {
		t.Fatal("marked managed Secret should be recognized")
	}
	if ownedManagedTLSSecret(secret, uuid.New()) {
		t.Fatal("different Project must not claim Secret")
	}
}
