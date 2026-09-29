package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// 旧入口端口在迁移期间必须继续存在，直到 HTTPRoute 转向稳定端口。
func TestServicePortsRetainLegacyRouteDuringTargetPortChanges(t *testing.T) {
	legacy := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
		Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP,
		TargetPort: intstr.FromString("http"),
	}}}}
	if got := routeBackendPort(legacy); got != 8080 {
		t.Fatalf("route should still use the existing Service port before publish: %d", got)
	}
	ports := servicePorts(legacy)
	if len(ports) != 2 || ports[0].Port != 80 || ports[1].Port != 8080 {
		t.Fatalf("new Service must expose stable and legacy ports: %#v", ports)
	}
	for _, port := range ports {
		if port.TargetPort.StrVal != "http" {
			t.Fatalf("Service port must follow the named container port: %#v", port)
		}
	}
	updated := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Port: 80, Protocol: corev1.ProtocolTCP},
		{Port: 8080, Protocol: corev1.ProtocolTCP},
	}}}
	if got := routeBackendPort(updated); got != 80 {
		t.Fatalf("route should switch only after stable Service port exists: %d", got)
	}
	if got := servicePorts(updated); len(got) != 2 || got[1].Port != 8080 {
		t.Fatalf("rollback/retry must preserve the old backend port: %#v", got)
	}
}

func TestRouteBackendPortDefaultsToStablePortBeforeFirstRelease(t *testing.T) {
	if got := routeBackendPort(nil); got != 80 {
		t.Fatalf("new route should await the stable Service port: %d", got)
	}
	if got := servicePorts(nil); len(got) != 1 || got[0].Port != 80 {
		t.Fatalf("first release should create stable Service port: %#v", got)
	}
}
