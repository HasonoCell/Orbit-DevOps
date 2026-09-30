package integration_test

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/accessworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/app"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/test/testsupport"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestAccessManagedTLSRunsThroughAPIQueueAndKind 从公开命令经 outbox/Asynq/Worker 到真实 HTTPS。
func TestAccessManagedTLSRunsThroughAPIQueueAndKind(t *testing.T) {
	if os.Getenv("ORBIT_DEVOPS_GATEWAY_E2E") != "1" {
		t.Skip("set ORBIT_DEVOPS_GATEWAY_E2E=1 for Kind gateway chain")
	}
	kubeconfig := os.Getenv("ORBIT_DEVOPS_KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("ORBIT_DEVOPS_KUBECONFIG is required")
	}
	cluster := "kind-" + environmentOrDefaultForAccess("ORBIT_DEVOPS_KIND_CLUSTER_NAME", "orbit-gateway-full")
	namespace := environmentOrDefaultForAccess("ORBIT_DEVOPS_NAMESPACE", "orbit-gateway-full")
	ctx := context.Background()
	base, err := kube.NewVerifiedLocalAdapter(ctx, kubeconfig, cluster, kube.Config{
		ClusterRef: cluster, Namespace: namespace, FieldManager: "orbit-gateway-chain-test",
		PollInterval: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("verify dedicated Kind cluster: %v", err)
	}
	const className = "orbit-gateway-acceptance"
	gateway, err := kube.NewGatewayAdapter(base, className)
	if err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	policy := access.IssuerPolicy{Kind: "Issuer", Name: "orbit-acceptance-selfsigned"}
	environment := newTestEnvironmentWithConfig(t, app.Dependencies{
		SecretVerifier: base, AccessObserver: gateway,
	}, func(config *app.Config) {
		config.LocalClusterRef, config.LocalNamespace = cluster, namespace
		config.GatewayClassName = className
		config.AccessIssuerPolicies = map[string]access.IssuerPolicy{"local": policy}
	})
	project := createProject(t, environment, "kind-chain-project")
	application := createApplication(t, environment, project.ID, "kind-chain-app")
	target := createAccessTarget(t, environment, application.ID, "kind-chain-target", 80)
	production := requestAccessDocument[deploymentTargetDocument](t, environment.server, http.MethodPost,
		"/api/v1/applications/"+application.ID+"/deployment-targets", "kind-chain-production-target",
		`{"stage":"production","replicas":1,"containerPort":80}`, http.StatusCreated)
	projectID, targetID := uuid.MustParse(project.ID), uuid.MustParse(target.ID)
	backend := kube.ResourceName(targetID)
	labels := map[string]string{kube.ManagedByLabel: kube.ManagedByValue,
		kube.ProjectIDLabel: project.ID, kube.TargetIDLabel: target.ID, "app": backend}
	if _, err := client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: backend, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "whoami", Image: "traefik/whoami:v1.11.0",
			ImagePullPolicy: corev1.PullIfNotPresent, Ports: []corev1.ContainerPort{{ContainerPort: 80}}}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create backend Pod: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Pods(namespace).Delete(context.Background(), backend, metav1.DeleteOptions{})
	})
	if _, err := client.CoreV1().Services(namespace).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: backend, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": backend},
			Ports: []corev1.ServicePort{{Port: 80}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create backend Service: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Services(namespace).Delete(context.Background(), backend, metav1.DeleteOptions{})
	})
	productionBackend := kube.ResourceName(uuid.MustParse(production.ID))
	productionLabels := map[string]string{kube.ManagedByLabel: kube.ManagedByValue,
		kube.ProjectIDLabel: project.ID, kube.TargetIDLabel: production.ID, "app": productionBackend}
	if _, err := client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: productionBackend, Namespace: namespace, Labels: productionLabels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "whoami", Image: "traefik/whoami:v1.11.0",
			ImagePullPolicy: corev1.PullIfNotPresent, Ports: []corev1.ContainerPort{{ContainerPort: 80}}}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create production backend Pod: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Pods(namespace).Delete(context.Background(), productionBackend, metav1.DeleteOptions{})
	})
	if _, err := client.CoreV1().Services(namespace).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: productionBackend, Namespace: namespace, Labels: productionLabels},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": productionBackend},
			Ports: []corev1.ServicePort{{Port: 80}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create production backend Service: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Services(namespace).Delete(context.Background(), productionBackend, metav1.DeleteOptions{})
	})
	db := openTestDatabase(t, environment.databaseURL)
	module, err := access.New(db, projectauth.New(db, nil), access.Config{
		ClusterRef: cluster, Namespace: namespace, GatewayClassName: className,
		IssuerPolicies: map[string]access.IssuerPolicy{"local": policy},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := accessworker.New(module, gateway, 60*time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, redisAddress := testsupport.StartRedis(t)
	queueConfig := internalEventConfig(redisAddress)
	queueConfig.Queue = "orbit-gateway-kind-chain"
	queueConfig.Topics = []string{accessworker.Topic}
	queueConfig.TaskTimeout = 60 * time.Second
	service, err := internalevent.NewService(queueConfig, internalevent.New(db), worker)
	if err != nil {
		t.Fatal(err)
	}
	stopEvents := startInternalEvents(t, service)
	// 真实 Gateway Worker 同时运行维护扫描；并发唤醒被租约合并时，由扫描保证最新修订最终收敛。
	maintenanceContext, stopMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan error, 1)
	go func() { maintenanceDone <- worker.Maintain(maintenanceContext, 500*time.Millisecond) }()
	t.Cleanup(func() {
		stopEvents()
		stopMaintenance()
		if err := <-maintenanceDone; err != nil {
			t.Errorf("stop gateway maintenance: %v", err)
		}
		// 断言失败时仍只清理本测试 Project 的入口资源，不触碰别的 Kind 对象。
		empty := access.Snapshot{ProjectID: projectID, ClusterRef: cluster, Namespace: namespace,
			GatewayClassName: className, Revision: 100}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if err := gateway.Reconcile(context.Background(), empty, func(context.Context) error { return nil }); err == nil {
				return
			}
			time.Sleep(time.Second)
		}
		t.Error("gateway resources did not clean up after integration test")
	})

	hostname := "chain-tls.orbit-gateway.test"
	hostPath := "/api/v1/projects/" + project.ID + "/access-hosts"
	host := requestAccessDocument[accessHostDocument](t, environment.server, http.MethodPost, hostPath,
		"kind-chain-host", fmt.Sprintf(`{"hostname":%q,"tlsMode":"managed","issuerPolicyKey":"local"}`, hostname),
		http.StatusCreated)
	route := requestAccessDocument[accessRouteDocument](t, environment.server, http.MethodPost,
		hostPath+"/"+host.ID+"/routes", "kind-chain-route",
		fmt.Sprintf(`{"pathPrefix":"/","deploymentTargetId":%q}`, target.ID), http.StatusCreated)
	productionHostname := "chain-prod.orbit-gateway.test"
	productionHost := requestAccessDocument[accessHostDocument](t, environment.server, http.MethodPost, hostPath,
		"kind-chain-production-host", fmt.Sprintf(`{"hostname":%q,"tlsMode":"managed","issuerPolicyKey":"local"}`, productionHostname),
		http.StatusCreated)
	productionRoute := requestAccessDocument[accessRouteDocument](t, environment.server, http.MethodPost,
		hostPath+"/"+productionHost.ID+"/routes", "kind-chain-production-route",
		fmt.Sprintf(`{"pathPrefix":"/","deploymentTargetId":%q}`, production.ID), http.StatusCreated)
	awaitAccessChain(t, db, module, gateway, projectID, uuid.MustParse(host.ID), 4, true)
	awaitAccessChain(t, db, module, gateway, projectID, uuid.MustParse(productionHost.ID), 4, true)
	proxyService := awaitGatewayProxyService(t, client, namespace, kube.GatewayName(projectID))
	httpPort, httpsPort := freeLocalPortForAccess(t), freeLocalPortForAccess(t)
	forwardCtx, stopForward := context.WithCancel(ctx)
	forward := exec.CommandContext(forwardCtx, "kubectl", "--kubeconfig", kubeconfig,
		"-n", "envoy-gateway-system", "port-forward", "svc/"+proxyService,
		fmt.Sprintf("%d:80", httpPort), fmt.Sprintf("%d:443", httpsPort))
	if err := forward.Start(); err != nil {
		t.Fatalf("port-forward Envoy: %v", err)
	}
	t.Cleanup(func() { stopForward(); _ = forward.Wait() })
	plain := &http.Client{Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	secure := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 仅测试本地自签名证书。
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpsPort))
		},
	}}
	testsupport.AwaitHTTPResponse(t, plain, fmt.Sprintf("http://127.0.0.1:%d/", httpPort), hostname,
		http.StatusMovedPermanently, "")
	testsupport.AwaitHTTPResponse(t, secure, "https://"+hostname+"/", hostname,
		http.StatusOK, "Hostname: "+backend)
	testsupport.AwaitHTTPResponse(t, secure, "https://"+productionHostname+"/", productionHostname,
		http.StatusOK, "Hostname: "+productionBackend)

	// 同一条进程链继续切换为管理员登记的已有 Secret，再验证 HTTPS 和安全清理。
	managed, err := client.CoreV1().Secrets(namespace).Get(ctx, kube.AccessTLSSecretName(uuid.MustParse(host.ID)), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read managed TLS Secret: %v", err)
	}
	statusResponse := requestJSON(t, environment.server, http.MethodGet,
		hostPath+"/"+host.ID+"/status", "", "")
	defer statusResponse.Body.Close()
	if statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("read managed Host status = %d", statusResponse.StatusCode)
	}
	statusBody, err := io.ReadAll(statusResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	var statusDocument struct {
		Sync struct {
			State string `json:"state"`
		} `json:"sync"`
		Controller struct {
			CertificateState string `json:"certificateState"`
			SecretState      string `json:"secretState"`
		} `json:"controller"`
		DNS struct {
			State string `json:"state"`
		} `json:"dns"`
	}
	if err := json.Unmarshal(statusBody, &statusDocument); err != nil {
		t.Fatal(err)
	}
	if statusDocument.Sync.State != "applied" || statusDocument.Controller.CertificateState != "ready" ||
		statusDocument.Controller.SecretState != "ready" || statusDocument.DNS.State == "verified" {
		t.Fatalf("status conflated sync, certificate, and DNS: %+v", statusDocument)
	}
	privateKey := managed.Data[corev1.TLSPrivateKeyKey]
	if len(privateKey) == 0 {
		t.Fatal("managed TLS Secret contains no private key")
	}
	if strings.Contains(string(statusBody), base64.StdEncoding.EncodeToString(privateKey)) ||
		strings.Contains(string(statusBody), "PRIVATE KEY") {
		t.Fatal("Host status exposed TLS private key")
	}
	externalName := "chain-external-tls-" + host.ID
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: externalName, Namespace: namespace},
		Type:       corev1.SecretTypeTLS, Data: managed.Data,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create external TLS Secret: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Secrets(namespace).Delete(context.Background(), externalName, metav1.DeleteOptions{})
	})
	binding := requestAccessDocument[accessSecretBindingDocument](t, environment.server, http.MethodPost,
		"/api/v1/platform/access-secret-bindings", "kind-chain-binding",
		fmt.Sprintf(`{"projectId":%q,"hostname":%q,"secretName":%q}`, project.ID, hostname, externalName),
		http.StatusCreated)
	updated := requestJSON(t, environment.server, http.MethodPatch, hostPath+"/"+host.ID,
		"kind-chain-switch-secret", fmt.Sprintf(`{"hostname":%q,"tlsMode":"existing_secret","secretBindingId":%q}`,
			hostname, binding.ID))
	defer updated.Body.Close()
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("switch Host to existing Secret status = %d", updated.StatusCode)
	}
	awaitAccessChain(t, db, module, gateway, projectID, uuid.MustParse(host.ID), 5, false)
	testsupport.AwaitHTTPResponse(t, secure, "https://"+hostname+"/", hostname,
		http.StatusOK, "Hostname: "+backend)
	deletedRoute := requestJSON(t, environment.server, http.MethodDelete,
		hostPath+"/"+host.ID+"/routes/"+route.ID, "kind-chain-delete-route", "")
	defer deletedRoute.Body.Close()
	if deletedRoute.StatusCode != http.StatusAccepted {
		t.Fatalf("delete Route status = %d", deletedRoute.StatusCode)
	}
	deletedHost := requestJSON(t, environment.server, http.MethodDelete,
		hostPath+"/"+host.ID, "kind-chain-delete-host", "")
	defer deletedHost.Body.Close()
	if deletedHost.StatusCode != http.StatusAccepted {
		t.Fatalf("delete Host status = %d", deletedHost.StatusCode)
	}
	awaitAccessChain(t, db, module, gateway, projectID, uuid.MustParse(productionHost.ID), 7, true)
	testsupport.AwaitHTTPResponse(t, secure, "https://"+productionHostname+"/", productionHostname,
		http.StatusOK, "Hostname: "+productionBackend)
	deletedProductionRoute := requestJSON(t, environment.server, http.MethodDelete,
		hostPath+"/"+productionHost.ID+"/routes/"+productionRoute.ID, "kind-chain-delete-production-route", "")
	defer deletedProductionRoute.Body.Close()
	if deletedProductionRoute.StatusCode != http.StatusAccepted {
		t.Fatalf("delete production Route status = %d", deletedProductionRoute.StatusCode)
	}
	deletedProductionHost := requestJSON(t, environment.server, http.MethodDelete,
		hostPath+"/"+productionHost.ID, "kind-chain-delete-production-host", "")
	defer deletedProductionHost.Body.Close()
	if deletedProductionHost.StatusCode != http.StatusAccepted {
		t.Fatalf("delete production Host status = %d", deletedProductionHost.StatusCode)
	}
	awaitAccessChain(t, db, module, gateway, projectID, uuid.Nil, 9, false)
	dynamic, err := base.DynamicClient()
	if err != nil {
		t.Fatal(err)
	}
	_, err = dynamic.Resource(schema.GroupVersionResource{Group: "gateway.networking.k8s.io",
		Version: "v1", Resource: "gateways"}).Namespace(namespace).Get(ctx, kube.GatewayName(projectID), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("project Gateway remained after Host deletion: %v", err)
	}
	if _, err := client.CoreV1().Secrets(namespace).Get(ctx, externalName, metav1.GetOptions{}); err != nil {
		t.Fatalf("external Secret removed by Orbit: %v", err)
	}
}

func environmentOrDefaultForAccess(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func awaitAccessChain(t *testing.T, db queryRowDB, module *access.Module, gateway *kube.GatewayAdapter,
	projectID, hostID uuid.UUID, revision int64, managed bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var applied int64
		var state string
		if err := db.QueryRow(`SELECT applied_revision,state FROM project_gateway_sync WHERE project_id=$1`,
			projectID).Scan(&applied, &state); err == nil && applied == revision && state == "applied" {
			if hostID == uuid.Nil {
				return
			}
			snapshot, err := module.LoadSnapshot(context.Background(), projectID)
			if err == nil {
				for _, host := range snapshot.Hosts {
					if host.Host.ID != hostID {
						continue
					}
					var routes []access.RouteSpec
					for _, route := range snapshot.Routes {
						if route.Route.HostID == hostID {
							routes = append(routes, route)
						}
					}
					observation, err := gateway.ObserveHost(context.Background(), snapshot, host, routes)
					if err == nil && observation.GatewayState == "ready" &&
						observation.ListenerState == "ready" && observation.SecretState == "ready" &&
						(!managed || observation.CertificateState == "ready") &&
						len(observation.Routes) == 1 && observation.Routes[0].Accepted == "ready" {
						return
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	var state, code string
	var applied, desired int64
	_ = db.QueryRow(`SELECT state, COALESCE(last_error_code,''), applied_revision, desired_revision
		FROM project_gateway_sync WHERE project_id=$1`, projectID).Scan(&state, &code, &applied, &desired)
	t.Fatalf("gateway chain did not reach revision %d with ready controller: state=%s code=%s applied=%d desired=%d",
		revision, state, code, applied, desired)
}

type queryRowDB interface {
	QueryRow(string, ...any) *sql.Row
}

func awaitGatewayProxyService(t *testing.T, client kubernetes.Interface, namespace, gatewayName string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		services, err := client.CoreV1().Services("envoy-gateway-system").List(context.Background(), metav1.ListOptions{})
		if err == nil {
			for _, service := range services.Items {
				if service.Labels["gateway.envoyproxy.io/owning-gateway-name"] == gatewayName &&
					service.Labels["gateway.envoyproxy.io/owning-gateway-namespace"] == namespace {
					return service.Name
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("Envoy Service was not created")
	return ""
}

func freeLocalPortForAccess(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
