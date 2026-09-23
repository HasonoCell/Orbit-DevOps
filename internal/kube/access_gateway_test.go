package kube

import (
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGatewayRendersDistinctTLSListenersAndRoutes(t *testing.T) {
	projectID, hostID, targetID, routeID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	host := access.HostSpec{Host: access.Host{ID: hostID, ProjectID: projectID, Hostname: "shop.example.com", TLSMode: "managed", Lifecycle: "active"},
		Issuer: access.IssuerPolicy{Kind: "ClusterIssuer", Name: "local-ca"}}
	route := access.RouteSpec{Route: access.Route{ID: routeID, HostID: hostID, DeploymentTargetID: targetID, PathPrefix: "/api", Lifecycle: "active"}, ContainerPort: 8080}
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
}
