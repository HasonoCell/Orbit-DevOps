package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/app"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/transport/httpapi"
	"github.com/HasonoCell/Orbit-DevOps/internal/webhook"
	"github.com/jmoiron/sqlx"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const integrationOrigin = "http://127.0.0.1:5173"
const integrationPassword = "fixture-only!Orbit-Integration-2026"

type testEnvironment struct {
	server              *httptest.Server
	databaseURL         string
	dependencies        app.Dependencies
	postgresContainerID string
	identities          *identity.Module
	adminCaller         identity.Caller
	users               map[string]identity.User
}

// serverForActor 使用正常本地登录取得独立 Cookie，不再用服务端配置伪造操作者。
func (e *testEnvironment) serverForActor(t *testing.T, actorID string) *httptest.Server {
	t.Helper()
	e.ensureActor(t, actorID)
	server := e.startServer(t)
	loginHTTPFixture(t, server, actorID, integrationPassword+"-completed")
	return server
}

func (e *testEnvironment) ensureActor(t *testing.T, loginName string) identity.User {
	t.Helper()
	if user, ok := e.users[loginName]; ok {
		return user
	}
	ctx := context.Background()
	user, err := e.identities.CreateLocalUser(ctx, identity.CreateLocalUserCommand{Caller: e.adminCaller,
		LoginName: loginName, DisplayName: loginName, TemporaryPassword: integrationPassword})
	if err != nil {
		t.Fatalf("create integration actor %q: %v", loginName, err)
	}
	login, err := e.identities.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: loginName,
		Password: integrationPassword, SourceIP: "127.0.0.2"})
	if err != nil {
		t.Fatalf("login temporary integration actor %q: %v", loginName, err)
	}
	caller, err := e.identities.ResolveSession(ctx, login.Token.CookieValue())
	if err != nil {
		t.Fatalf("resolve integration actor %q: %v", loginName, err)
	}
	if err := e.identities.ChangePassword(ctx, identity.ChangePasswordCommand{Caller: caller,
		CurrentPassword: integrationPassword, NewPassword: integrationPassword + "-completed", SourceIP: "127.0.0.2"}); err != nil {
		t.Fatalf("complete integration actor %q: %v", loginName, err)
	}
	e.users[loginName] = user
	return user
}

// callerForActor 返回已经完成临时密码修改的正式会话，供绕过 HTTP 的模块级集成测试使用。
func (e *testEnvironment) callerForActor(t *testing.T, loginName string) identity.Caller {
	t.Helper()
	if loginName == "local-developer" {
		return e.adminCaller
	}
	e.ensureActor(t, loginName)
	login, err := e.identities.LoginLocal(context.Background(), identity.LocalLoginCommand{
		LoginName: loginName, Password: integrationPassword + "-completed", SourceIP: "127.0.0.2",
	})
	if err != nil {
		t.Fatalf("login integration caller %q: %v", loginName, err)
	}
	caller, err := e.identities.ResolveSession(context.Background(), login.Token.CookieValue())
	if err != nil {
		t.Fatalf("resolve integration caller %q: %v", loginName, err)
	}
	return caller
}

func (e *testEnvironment) startServer(t *testing.T) *httptest.Server {
	t.Helper()
	runtime, err := app.NewWithDependencies(context.Background(), integrationAppConfig(e.databaseURL), e.dependencies)
	if err != nil {
		t.Fatalf("start Orbit-DevOps integration instance: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	server := httptest.NewServer(runtime.Handler())
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal("create integration Cookie jar")
	}
	server.Client().Jar = jar
	t.Cleanup(server.Close)
	return server
}

type projectDocument struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

type applicationDocument struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

type deploymentTargetDocument struct {
	ProjectID     string    `json:"-"`
	ID            string    `json:"id"`
	ApplicationID string    `json:"applicationId"`
	Stage         string    `json:"stage"`
	ClusterRef    string    `json:"clusterRef"`
	Namespace     string    `json:"namespace"`
	Replicas      int       `json:"replicas"`
	ContainerPort int       `json:"containerPort"`
	CreatedBy     string    `json:"createdBy"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type releaseDocument struct {
	ID                  string                `json:"id"`
	DeploymentTargetID  string                `json:"deploymentTargetId"`
	ImageReference      string                `json:"imageReference"`
	ImageArtifactID     *string               `json:"imageArtifactId"`
	TargetSnapshot      releaseTargetSnapshot `json:"targetSnapshot"`
	RollbackOfReleaseID *string               `json:"rollbackOfReleaseId"`
	CreatedBy           string                `json:"createdBy"`
	CreatedAt           time.Time             `json:"createdAt"`
}

type releaseDetailDocument struct {
	Release             releaseDocument              `json:"release"`
	SnapshotDifferences []snapshotDifferenceDocument `json:"snapshotDifferences"`
	ReleaseOperation    operationDocument            `json:"releaseOperation"`
	AuditTimeline       []auditRecordDocument        `json:"auditTimeline"`
}

type snapshotDifferenceDocument struct {
	Field        string `json:"field"`
	ReleaseValue string `json:"releaseValue"`
	CurrentValue string `json:"currentValue"`
}

type auditRecordDocument struct {
	ID         string         `json:"id"`
	ActorID    string         `json:"actorId"`
	ActorKind  string         `json:"actorKind"`
	Action     string         `json:"action"`
	TargetType string         `json:"targetType"`
	TargetID   string         `json:"targetId"`
	Summary    map[string]any `json:"summary"`
	CreatedAt  time.Time      `json:"createdAt"`
}

type operationSummaryDocument struct {
	ID           string                                  `json:"id"`
	Status       releaseoperation.ReleaseOperationStatus `json:"status"`
	AttemptCount int                                     `json:"attemptCount"`
	ErrorCode    *string                                 `json:"errorCode"`
	ErrorSummary *string                                 `json:"errorSummary"`
	QueuedAt     time.Time                               `json:"queuedAt"`
	StartedAt    *time.Time                              `json:"startedAt"`
	FinishedAt   *time.Time                              `json:"finishedAt"`
}

type releaseHistoryItemDocument struct {
	Release          releaseDocument          `json:"release"`
	ReleaseOperation operationSummaryDocument `json:"releaseOperation"`
}

type releaseHistoryPageDocument struct {
	Items      []releaseHistoryItemDocument `json:"items"`
	NextCursor *string                      `json:"nextCursor"`
}

type releaseTargetSnapshot struct {
	ProjectID     string `json:"projectId"`
	ApplicationID string `json:"applicationId"`
	Stage         string `json:"stage"`
	ClusterRef    string `json:"clusterRef"`
	Namespace     string `json:"namespace"`
	Replicas      int    `json:"replicas"`
	ContainerPort int    `json:"containerPort"`
}

type operationDocument struct {
	ID                  string                                  `json:"id"`
	Type                string                                  `json:"type"`
	ReleaseID           string                                  `json:"releaseId"`
	DeploymentTargetID  string                                  `json:"deploymentTargetId"`
	CreatedBy           string                                  `json:"createdBy"`
	IdempotencyKey      string                                  `json:"idempotencyKey"`
	Status              releaseoperation.ReleaseOperationStatus `json:"status"`
	AttemptCount        int                                     `json:"attemptCount"`
	AutomaticRetryCount int                                     `json:"automaticRetryCount"`
	RecoveryRequired    bool                                    `json:"recoveryRequired"`
	ErrorCode           *string                                 `json:"errorCode"`
	ErrorSummary        *string                                 `json:"errorSummary"`
	RetryDisposition    *string                                 `json:"retryDisposition"`
	QueuedAt            time.Time                               `json:"queuedAt"`
	AvailableAt         time.Time                               `json:"availableAt"`
	CreatedAt           time.Time                               `json:"createdAt"`
	UpdatedAt           time.Time                               `json:"updatedAt"`
	StartedAt           *time.Time                              `json:"startedAt"`
	FinishedAt          *time.Time                              `json:"finishedAt"`
	Attempts            []operationAttemptDocument              `json:"attempts"`
}

type operationAttemptDocument struct {
	ID               string                         `json:"id"`
	Number           int                            `json:"number"`
	WorkerID         string                         `json:"workerId"`
	Status           releaseoperation.AttemptStatus `json:"status"`
	ErrorCode        *string                        `json:"errorCode"`
	ErrorSummary     *string                        `json:"errorSummary"`
	RetryDisposition *string                        `json:"retryDisposition"`
	StartedAt        time.Time                      `json:"startedAt"`
	FinishedAt       *time.Time                     `json:"finishedAt"`
}

type releaseAcceptanceDocument struct {
	Release          releaseDocument   `json:"release"`
	ReleaseOperation operationDocument `json:"releaseOperation"`
}

func newTestEnvironment(t *testing.T) *testEnvironment {
	return newTestEnvironmentWithDependencies(t, app.Dependencies{})
}

func newTestEnvironmentWithDependencies(
	t *testing.T,
	dependencies app.Dependencies,
) *testEnvironment {
	t.Helper()

	ctx := context.Background()
	postgresContainer, err := postgres.Run(
		ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("orbitdevops"),
		postgres.WithUsername("orbitdevops"),
		postgres.WithPassword("orbitdevops"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := postgresContainer.Terminate(context.Background()); err != nil {
			t.Errorf("terminate PostgreSQL: %v", err)
		}
	})

	databaseURL, err := postgresContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get PostgreSQL connection string: %v", err)
	}

	if err := database.Migrate(databaseURL); err != nil {
		t.Fatalf("migrate integration database: %v", err)
	}
	identityDB, err := sqlx.ConnectContext(ctx, "pgx", databaseURL)
	if err != nil {
		t.Fatal("connect integration identity fixture")
	}
	t.Cleanup(func() { _ = identityDB.Close() })
	identities, err := identity.New(identityDB, projectauth.NewOwnershipGuard())
	if err != nil {
		t.Fatal("construct integration identity fixture")
	}
	admin, err := identities.InitializeAdmin(ctx, identity.InitializeAdminCommand{LoginName: "local-developer",
		DisplayName: "Integration administrator", Password: integrationPassword, MaintenanceRef: "integration-fixture"})
	if err != nil && !errors.Is(err, identity.ErrAlreadyInitialized) {
		t.Fatalf("initialize integration administrator: %v", err)
	}
	adminLogin, err := identities.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: "local-developer",
		Password: integrationPassword, SourceIP: "127.0.0.2"})
	if err != nil {
		t.Fatalf("login integration administrator: %v", err)
	}
	adminCaller, err := identities.ResolveSession(ctx, adminLogin.Token.CookieValue())
	if err != nil {
		t.Fatal("resolve integration administrator")
	}
	runtime, err := app.NewWithDependencies(ctx, integrationAppConfig(databaseURL), dependencies)
	if err != nil {
		t.Fatalf("start Orbit-DevOps: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close Orbit-DevOps: %v", err)
		}
	})

	server := httptest.NewServer(runtime.Handler())
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal("create integration Cookie jar")
	}
	server.Client().Jar = jar
	t.Cleanup(server.Close)
	loginHTTPFixture(t, server, "local-developer", integrationPassword)

	return &testEnvironment{
		server: server, databaseURL: databaseURL, dependencies: dependencies,
		postgresContainerID: postgresContainer.GetContainerID(),
		identities:          identities, adminCaller: adminCaller,
		users: map[string]identity.User{"local-developer": admin},
	}
}

func integrationAppConfig(databaseURL string) app.Config {
	return app.Config{DatabaseURL: databaseURL,
		BrowserSecurity: httpapi.BrowserSecurityConfig{ExternalURL: integrationOrigin, AllowLoopbackHTTP: true},
		LocalClusterRef: "kind-orbit-devops-s1", LocalNamespace: "orbit-devops-s1",
		BuildAllowedGitHosts: []string{"github.com"}, BuildPlatform: "linux/amd64",
		BuildRegistryHost: "registry.example", BuildRegistryPrefix: "orbit-devops",
		WebhookConfig: webhook.Config{Endpoints: map[string]webhook.EndpointSecrets{
			"integration": {Current: "integration-webhook-secret"},
		}}, MigrateOnBoot: false}
}

func loginHTTPFixture(t *testing.T, server *httptest.Server, loginName, password string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"loginName": loginName, "password": password})
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		server.URL+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatal("build integration login request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", integrationOrigin)
	request.Header.Set("X-Orbit-CSRF", "1")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("login integration actor %q: %v", loginName, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login integration actor %q status = %d", loginName, response.StatusCode)
	}
}

func (e *testEnvironment) postProject(
	t *testing.T,
	idempotencyKey string,
	requestBody string,
) *http.Response {
	t.Helper()
	return e.postJSON(t, "/api/v1/projects", idempotencyKey, requestBody)
}

func (e *testEnvironment) postJSON(
	t *testing.T,
	path string,
	idempotencyKey string,
	requestBody string,
) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		e.server.URL+path,
		bytes.NewBufferString(requestBody),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set("Origin", integrationOrigin)
	request.Header.Set("X-Orbit-CSRF", "1")

	response, err := e.server.Client().Do(request)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	return response
}

func (e *testEnvironment) putJSON(
	t *testing.T,
	path string,
	idempotencyKey string,
	requestBody string,
) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		e.server.URL+path,
		bytes.NewBufferString(requestBody),
	)
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set("Origin", integrationOrigin)
	request.Header.Set("X-Orbit-CSRF", "1")

	response, err := e.server.Client().Do(request)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	request.Header.Set("Origin", integrationOrigin)
	return response
}

func (e *testEnvironment) get(t *testing.T, path string) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		e.server.URL+path,
		nil,
	)
	if err != nil {
		t.Fatalf("build GET request: %v", err)
	}

	response, err := e.server.Client().Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}

	return response
}

func decodeProject(t *testing.T, response *http.Response) projectDocument {
	t.Helper()

	var project projectDocument
	if err := json.NewDecoder(response.Body).Decode(&project); err != nil {
		t.Fatalf("decode project response: %v", err)
	}

	return project
}

func decodeApplication(t *testing.T, response *http.Response) applicationDocument {
	t.Helper()

	var application applicationDocument
	if err := json.NewDecoder(response.Body).Decode(&application); err != nil {
		t.Fatalf("decode application response: %v", err)
	}

	return application
}

func decodeDeploymentTarget(
	t *testing.T,
	response *http.Response,
) deploymentTargetDocument {
	t.Helper()

	var target deploymentTargetDocument
	if err := json.NewDecoder(response.Body).Decode(&target); err != nil {
		t.Fatalf("decode deployment target response: %v", err)
	}

	return target
}

func decodeReleaseAcceptance(
	t *testing.T,
	response *http.Response,
) releaseAcceptanceDocument {
	t.Helper()

	var acceptance releaseAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&acceptance); err != nil {
		t.Fatalf("decode release acceptance response: %v", err)
	}

	return acceptance
}

func decodeRelease(t *testing.T, response *http.Response) releaseDocument {
	t.Helper()

	return decodeReleaseDetail(t, response).Release
}

func decodeReleaseDetail(t *testing.T, response *http.Response) releaseDetailDocument {
	t.Helper()

	var detail releaseDetailDocument
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatalf("decode release response: %v", err)
	}
	return detail
}

func decodeReleaseOperation(t *testing.T, response *http.Response) operationDocument {
	t.Helper()

	var operation operationDocument
	if err := json.NewDecoder(response.Body).Decode(&operation); err != nil {
		t.Fatalf("decode operation response: %v", err)
	}

	return operation
}
