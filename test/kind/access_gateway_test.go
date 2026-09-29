package kind_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/HasonoCell/Orbit-DevOps/test/testsupport"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestKindAccessGatewayAcceptance 使用独立 Kind 集群中的真实控制器验证 Orbit 生成的入口资源和流量。
// 该测试需要预装 Envoy Gateway、cert-manager、测试 Issuer、whoami 镜像和 cmctl，不使用默认 context。
func TestKindAccessGatewayAcceptance(t *testing.T) {
	if os.Getenv("ORBIT_DEVOPS_GATEWAY_E2E") != "1" {
		t.Skip("set ORBIT_DEVOPS_GATEWAY_E2E=1 to run gateway acceptance")
	}
	kubeconfig := os.Getenv("ORBIT_DEVOPS_KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("ORBIT_DEVOPS_KUBECONFIG is required")
	}
	cluster := "kind-" + environmentOrDefault("ORBIT_DEVOPS_KIND_CLUSTER_NAME", "orbit-gateway-acceptance")
	namespace := environmentOrDefault("ORBIT_DEVOPS_NAMESPACE", "orbit-gateway-acceptance")
	base, err := kube.NewVerifiedLocalAdapter(context.Background(), kubeconfig, cluster, kube.Config{
		ClusterRef: cluster, Namespace: namespace, FieldManager: "orbit-gateway-acceptance", PollInterval: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("verify local Kind adapter: %v", err)
	}
	adapter, err := kube.NewGatewayAdapter(base, "orbit-gateway-acceptance")
	if err != nil {
		t.Fatalf("create gateway adapter: %v", err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	projectID, targetID, apiTargetID := uuid.New(), uuid.New(), uuid.New()
	backendName := kube.ResourceName(targetID)
	apiBackendName := kube.ResourceName(apiTargetID)
	ctx := context.Background()
	createAccessBackend(t, client, namespace, projectID, targetID)
	createAccessBackend(t, client, namespace, projectID, apiTargetID)
	issuerKey := "local"
	httpHost := access.HostSpec{Host: access.Host{ID: uuid.New(), ProjectID: projectID,
		ClusterRef: cluster, Namespace: namespace, Hostname: "http.orbit-gateway.test",
		TLSMode: "http_only", Lifecycle: "active"}}
	tlsHost := access.HostSpec{Host: access.Host{ID: uuid.New(), ProjectID: projectID,
		ClusterRef: cluster, Namespace: namespace, Hostname: "tls.orbit-gateway.test",
		TLSMode: "managed", IssuerPolicyKey: &issuerKey, Lifecycle: "active"},
		Issuer: access.IssuerPolicy{Kind: "Issuer", Name: "orbit-acceptance-selfsigned"}}
	httpRoute := access.RouteSpec{Route: access.Route{ID: uuid.New(), HostID: httpHost.Host.ID,
		DeploymentTargetID: targetID, PathPrefix: "/", Lifecycle: "active"}, ServicePort: 80}
	apiRoute := access.RouteSpec{Route: access.Route{ID: uuid.New(), HostID: httpHost.Host.ID,
		DeploymentTargetID: apiTargetID, PathPrefix: "/api", Lifecycle: "active"}, ServicePort: 80}
	tlsRoute := access.RouteSpec{Route: access.Route{ID: uuid.New(), HostID: tlsHost.Host.ID,
		DeploymentTargetID: targetID, PathPrefix: "/", Lifecycle: "active"}, ServicePort: 80}
	snapshot := access.Snapshot{ProjectID: projectID, ClusterRef: cluster, Namespace: namespace,
		GatewayClassName: "orbit-gateway-acceptance", Revision: 1,
		Hosts: []access.HostSpec{httpHost, tlsHost}, Routes: []access.RouteSpec{httpRoute, apiRoute, tlsRoute}}
	if err := adapter.Reconcile(ctx, snapshot, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("reconcile gateway: %v", err)
	}
	externalSecretName := ""
	t.Cleanup(func() {
		snapshot.Hosts, snapshot.Routes, snapshot.Revision = nil, nil, 2
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			if err := adapter.Reconcile(context.Background(), snapshot, func(context.Context) error { return nil }); err == nil {
				if externalSecretName != "" {
					_ = client.CoreV1().Secrets(namespace).Delete(context.Background(), externalSecretName, metav1.DeleteOptions{})
				}
				return
			}
			time.Sleep(time.Second)
		}
		t.Error("gateway resources did not clean up within 45s")
	})

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		observation, err := adapter.ObserveHost(ctx, snapshot, tlsHost, []access.RouteSpec{tlsRoute})
		if err == nil && observation.GatewayState == "ready" && observation.ListenerState == "ready" &&
			observation.CertificateState == "ready" && observation.SecretState == "ready" &&
			len(observation.Routes) == 1 && observation.Routes[0].Accepted == "ready" &&
			observation.Routes[0].ResolvedRefs == "ready" {
			break
		}
		if time.Now().Add(time.Second).After(deadline) {
			t.Fatalf("TLS controller did not become ready: observation=%+v err=%v", observation, err)
		}
		time.Sleep(time.Second)
	}
	if err := waitForBackend(ctx, client, namespace, backendName, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForBackend(ctx, client, namespace, apiBackendName, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	proxyService, err := findEnvoyService(ctx, client, "envoy-gateway-system", namespace, kube.GatewayName(projectID))
	if err != nil {
		t.Fatal(err)
	}
	forwardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	portForward := exec.CommandContext(forwardCtx, "kubectl", "--kubeconfig", kubeconfig,
		"-n", "envoy-gateway-system", "port-forward", "svc/"+proxyService, "18080:80", "18443:443")
	if err := portForward.Start(); err != nil {
		t.Fatalf("start port-forward: %v", err)
	}
	defer func() { cancel(); _ = portForward.Wait() }()
	check := func(url, host string, client *http.Client, status int, body string) {
		testsupport.AwaitHTTPResponse(t, client, url, host, status, body)
	}
	plain := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	check("http://127.0.0.1:18080/", httpHost.Host.Hostname, plain, http.StatusOK, "Hostname: "+backendName)
	check("http://127.0.0.1:18080/api/users", httpHost.Host.Hostname, plain, http.StatusOK, "Hostname: "+apiBackendName)
	check("http://127.0.0.1:18080/", tlsHost.Host.Hostname, plain, http.StatusMovedPermanently, "")
	secure := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 仅测试自签名证书，绝不用于产品请求。
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:18443")
		},
	}}
	check("https://tls.orbit-gateway.test/", tlsHost.Host.Hostname, secure, http.StatusOK, "Hostname: "+backendName)
	// 手动触发 cert-manager 的正式续期流程；证书必须真的轮换，HTTPS 仍可用。
	cmctl := os.Getenv("ORBIT_DEVOPS_CMCTL")
	if cmctl == "" {
		t.Fatal("ORBIT_DEVOPS_CMCTL is required for certificate renewal acceptance")
	}
	beforeRenewal, err := client.CoreV1().Secrets(namespace).Get(ctx, kube.AccessTLSSecretName(tlsHost.Host.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read certificate before renewal: %v", err)
	}
	oldCertificate := append([]byte(nil), beforeRenewal.Data[corev1.TLSCertKey]...)
	renew := exec.CommandContext(ctx, cmctl, "renew", kube.AccessCertificateName(tlsHost.Host.ID),
		"--kubeconfig", kubeconfig, "--namespace", namespace)
	if output, err := renew.CombinedOutput(); err != nil {
		t.Fatalf("trigger cert-manager renewal: %v: %s", err, output)
	}
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		current, readErr := client.CoreV1().Secrets(namespace).Get(ctx, kube.AccessTLSSecretName(tlsHost.Host.ID), metav1.GetOptions{})
		observation, observeErr := adapter.ObserveHost(ctx, snapshot, tlsHost, []access.RouteSpec{tlsRoute})
		if readErr == nil && observeErr == nil && !bytes.Equal(current.Data[corev1.TLSCertKey], oldCertificate) &&
			observation.CertificateState == "ready" && observation.SecretState == "ready" {
			break
		}
		time.Sleep(time.Second)
	}
	renewedSecret, err := client.CoreV1().Secrets(namespace).Get(ctx, kube.AccessTLSSecretName(tlsHost.Host.ID), metav1.GetOptions{})
	if err != nil || bytes.Equal(renewedSecret.Data[corev1.TLSCertKey], oldCertificate) {
		t.Fatalf("certificate was not renewed: secret=%v error=%v", renewedSecret, err)
	}
	awaitServedTLSCertificate(t, "127.0.0.1:18443", tlsHost.Host.Hostname,
		renewedSecret.Data[corev1.TLSCertKey])
	check("https://tls.orbit-gateway.test/", tlsHost.Host.Hostname, secure, http.StatusOK, "Hostname: "+backendName)
	// 同名外部 Route 使一轮 Apply 部分失败；移除冲突后，原集合重试必须收敛。
	dynamic, err := base.DynamicClient()
	if err != nil {
		t.Fatal(err)
	}
	routeResource := dynamic.Resource(schema.GroupVersionResource{Group: "gateway.networking.k8s.io",
		Version: "v1", Resource: "httproutes"}).Namespace(namespace)
	conflictID := uuid.New()
	conflictName := kube.AccessRouteName(conflictID)
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{"name": conflictName, "namespace": namespace,
			"labels": map[string]any{"app.kubernetes.io/managed-by": "external-fixture"}},
		"spec": map[string]any{"parentRefs": []any{map[string]any{"name": kube.GatewayName(projectID)}},
			"hostnames": []any{"foreign.orbit-gateway.test"},
			"rules": []any{map[string]any{"backendRefs": []any{map[string]any{"name": backendName,
				"port": int64(80)}}}}},
	}}
	if _, err := routeResource.Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create foreign Route: %v", err)
	}
	t.Cleanup(func() { _ = routeResource.Delete(context.Background(), conflictName, metav1.DeleteOptions{}) })
	conflictRoute := access.RouteSpec{Route: access.Route{ID: conflictID, HostID: httpHost.Host.ID,
		DeploymentTargetID: targetID, PathPrefix: "/conflict", Lifecycle: "active"}, ServicePort: 80}
	snapshot.Routes = append(snapshot.Routes, conflictRoute)
	snapshot.Revision++
	if err := adapter.Reconcile(ctx, snapshot, func(context.Context) error { return nil }); !errors.Is(err, kube.ErrAccessOwnership) {
		t.Fatalf("foreign Route was not protected: %v", err)
	}
	if existing, err := routeResource.Get(ctx, conflictName, metav1.GetOptions{}); err != nil ||
		existing.GetLabels()["app.kubernetes.io/managed-by"] != "external-fixture" {
		t.Fatalf("foreign Route was changed: resource=%v error=%v", existing, err)
	}
	if err := routeResource.Delete(ctx, conflictName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("remove test conflict: %v", err)
	}
	if err := adapter.Reconcile(ctx, snapshot, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("retry partial Apply: %v", err)
	}
	check("http://127.0.0.1:18080/conflict", httpHost.Host.Hostname, plain, http.StatusOK,
		"Hostname: "+backendName)
	snapshot.Routes = snapshot.Routes[:len(snapshot.Routes)-1]
	snapshot.Revision++
	if err := adapter.Reconcile(ctx, snapshot, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("remove recovered Route: %v", err)
	}
	// 无效 Issuer 使证书保持未就绪；修正策略后同一 Host 继续签发。
	badIssuerKey := "missing"
	failingHost := access.HostSpec{Host: access.Host{ID: uuid.New(), ProjectID: projectID,
		ClusterRef: cluster, Namespace: namespace, Hostname: "recover.orbit-gateway.test",
		TLSMode: "managed", IssuerPolicyKey: &badIssuerKey, Lifecycle: "active"},
		Issuer: access.IssuerPolicy{Kind: "Issuer", Name: "missing-test-issuer"}}
	snapshot.Hosts = append(snapshot.Hosts, failingHost)
	snapshot.Revision++
	if err := adapter.Reconcile(ctx, snapshot, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("declare pending Certificate: %v", err)
	}
	observation, err := adapter.ObserveHost(ctx, snapshot, failingHost, nil)
	if err != nil || observation.CertificateState != "not_ready" || observation.SecretState != "not_ready" {
		t.Fatalf("failed issuer was hidden: observation=%+v error=%v", observation, err)
	}
	failingHost.Issuer = access.IssuerPolicy{Kind: "Issuer", Name: "orbit-acceptance-selfsigned"}
	snapshot.Hosts[len(snapshot.Hosts)-1] = failingHost
	snapshot.Revision++
	if err := adapter.Reconcile(ctx, snapshot, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("correct Certificate issuer: %v", err)
	}
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		observation, err = adapter.ObserveHost(ctx, snapshot, failingHost, nil)
		if err == nil && observation.CertificateState == "ready" && observation.SecretState == "ready" {
			break
		}
		time.Sleep(time.Second)
	}
	if observation.CertificateState != "ready" || observation.SecretState != "ready" {
		t.Fatalf("Certificate did not recover after issuer correction: %+v error=%v", observation, err)
	}
	snapshot.Hosts = snapshot.Hosts[:len(snapshot.Hosts)-1]
	snapshot.Revision++
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if err := adapter.Reconcile(ctx, snapshot, func(context.Context) error { return nil }); err == nil {
			break
		} else if time.Now().Add(time.Second).After(deadline) {
			t.Fatalf("clean recovered Certificate: %v", err)
		}
		time.Sleep(time.Second)
	}
	if observation, err := adapter.ObserveHost(ctx, snapshot, httpHost, []access.RouteSpec{httpRoute, apiRoute}); err != nil ||
		observation.GatewayState != "ready" || len(observation.Routes) != 2 ||
		observation.Routes[0].Accepted != "ready" || observation.Routes[1].Accepted != "ready" {
		t.Fatalf("HTTP controller observation=%+v err=%v", observation, err)
	}
	// 切换到管理员已登记的外部 Secret 后，旧托管证书应退出，外部 Secret 不得被 Orbit 删除。
	managedSecret, err := client.CoreV1().Secrets(namespace).Get(ctx, kube.AccessTLSSecretName(tlsHost.Host.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read managed TLS Secret: %v", err)
	}
	externalSecretName = "external-tls-" + tlsHost.Host.ID.String()
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: externalSecretName, Namespace: namespace},
		Type:       corev1.SecretTypeTLS, Data: beforeRenewal.Data,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create external TLS Secret: %v", err)
	}
	if err := base.VerifyTLSSecret(ctx, namespace, externalSecretName, tlsHost.Host.Hostname); err != nil {
		t.Fatalf("verify external TLS Secret: %v", err)
	}
	tlsHost.Host.TLSMode, tlsHost.Host.IssuerPolicyKey = "existing_secret", nil
	tlsHost.SecretName, tlsHost.BindingActive = externalSecretName, true
	snapshot.Hosts[1], snapshot.Revision = tlsHost, snapshot.Revision+1
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if err := adapter.Reconcile(ctx, snapshot, func(context.Context) error { return nil }); err == nil {
			break
		} else if time.Now().Add(time.Second).After(deadline) {
			t.Fatalf("switch to external TLS Secret: %v", err)
		}
		time.Sleep(time.Second)
	}
	if _, err := client.CoreV1().Secrets(namespace).Get(ctx, externalSecretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("external TLS Secret was removed: %v", err)
	}
	if _, err := client.CoreV1().Secrets(namespace).Get(ctx, kube.AccessTLSSecretName(tlsHost.Host.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old managed TLS Secret remains or lookup failed: %v", err)
	}
	if observation, err := adapter.ObserveHost(ctx, snapshot, tlsHost, []access.RouteSpec{tlsRoute}); err != nil ||
		observation.CertificateState != "not_applicable" || observation.SecretState != "ready" {
		t.Fatalf("external TLS controller observation=%+v err=%v", observation, err)
	}
	check("https://tls.orbit-gateway.test/", tlsHost.Host.Hostname, secure, http.StatusOK, "Hostname: "+backendName)
	awaitServedTLSCertificate(t, "127.0.0.1:18443", tlsHost.Host.Hostname,
		beforeRenewal.Data[corev1.TLSCertKey])
	// 外部证书轮换由其所有者更新 Secret；Orbit 只回读引用，不接管或删除它。
	external, err := client.CoreV1().Secrets(namespace).Get(ctx, externalSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	external.Data = managedSecret.Data
	if _, err := client.CoreV1().Secrets(namespace).Update(ctx, external, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("rotate external TLS Secret: %v", err)
	}
	if err := base.VerifyTLSSecret(ctx, namespace, externalSecretName, tlsHost.Host.Hostname); err != nil {
		t.Fatalf("verify rotated external Secret: %v", err)
	}
	awaitServedTLSCertificate(t, "127.0.0.1:18443", tlsHost.Host.Hostname,
		managedSecret.Data[corev1.TLSCertKey])
	check("https://tls.orbit-gateway.test/", tlsHost.Host.Hostname, secure, http.StatusOK, "Hostname: "+backendName)
	if err := client.CoreV1().Secrets(namespace).Delete(ctx, externalSecretName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("remove external Secret for failure injection: %v", err)
	}
	observation, err = adapter.ObserveHost(ctx, snapshot, tlsHost, []access.RouteSpec{tlsRoute})
	if err != nil || observation.SecretState != "not_ready" {
		t.Fatalf("missing external Secret was hidden: observation=%+v error=%v", observation, err)
	}
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: externalSecretName, Namespace: namespace},
		Type:       corev1.SecretTypeTLS, Data: managedSecret.Data,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("restore external TLS Secret: %v", err)
	}
	check("https://tls.orbit-gateway.test/", tlsHost.Host.Hostname, secure, http.StatusOK, "Hostname: "+backendName)
}

func awaitServedTLSCertificate(t *testing.T, address, hostname string, certificatePEM []byte) {
	t.Helper()
	block, _ := pem.Decode(certificatePEM)
	if block == nil {
		t.Fatal("TLS Secret contains no certificate")
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", address,
			&tls.Config{ServerName: hostname, InsecureSkipVerify: true}) // 仅测试本地自签名证书。
		if err == nil {
			state := connection.ConnectionState()
			_ = connection.Close()
			if len(state.PeerCertificates) > 0 && bytes.Equal(state.PeerCertificates[0].Raw, block.Bytes) {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("Envoy did not serve expected certificate for %s", hostname)
}

func createAccessBackend(t *testing.T, client kubernetes.Interface, namespace string, projectID, targetID uuid.UUID) {
	t.Helper()
	name := kube.ResourceName(targetID)
	labels := map[string]string{kube.ManagedByLabel: kube.ManagedByValue,
		kube.ProjectIDLabel: projectID.String(), kube.TargetIDLabel: targetID.String(), "app": name}
	if _, err := client.CoreV1().Pods(namespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "whoami", Image: "traefik/whoami:v1.11.0",
			ImagePullPolicy: corev1.PullIfNotPresent, Ports: []corev1.ContainerPort{{ContainerPort: 80}}}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create backend Pod: %v", err)
	}
	t.Cleanup(func() { _ = client.CoreV1().Pods(namespace).Delete(context.Background(), name, metav1.DeleteOptions{}) })
	if _, err := client.CoreV1().Services(namespace).Create(context.Background(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{{Port: 80}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create backend Service: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Services(namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

func waitForBackend(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && pod.Status.Phase == corev1.PodRunning {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("backend Pod %s did not start", name)
}

func findEnvoyService(ctx context.Context, client kubernetes.Interface, serviceNamespace, gatewayNamespace, gatewayName string) (string, error) {
	services, err := client.CoreV1().Services(serviceNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, service := range services.Items {
		if service.Labels["gateway.envoyproxy.io/owning-gateway-name"] == gatewayName &&
			service.Labels["gateway.envoyproxy.io/owning-gateway-namespace"] == gatewayNamespace {
			return service.Name, nil
		}
	}
	return "", fmt.Errorf("Envoy Service for Gateway %s not found", gatewayName)
}
