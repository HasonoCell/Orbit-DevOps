package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/app"
	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
)

// staticRuntimeSource 只在诊断模块 seam 返回已知的 Kubernetes 点时证据。
type staticRuntimeSource struct {
	observation diagnostics.RuntimeObservation
}

type matchingRuntimeSource struct{}

func (matchingRuntimeSource) ObserveRelease(
	_ context.Context,
	query diagnostics.RuntimeQuery,
) diagnostics.RuntimeObservation {
	metadata := diagnostics.ObservationMetadata{
		Source: diagnostics.SourceKubernetes, ObservedAt: time.Now().UTC(),
		Status: diagnostics.ObservationComplete, ErrorCategories: []string{},
	}
	return diagnostics.RuntimeObservation{
		Workload: diagnostics.WorkloadObservation{
			Metadata: metadata,
			Deployment: &diagnostics.DeploymentEvidence{
				Name: "application", UID: "deployment-uid",
				OwnershipMatches: true, ReleaseID: &query.ReleaseID,
			},
			Pods: []diagnostics.PodEvidence{},
		},
		Events: diagnostics.EventObservation{Metadata: metadata, Items: []diagnostics.EventEvidence{}},
	}
}

type staticRuntimeLogSource struct {
	result diagnostics.RuntimeLogResult
	err    error
}

func (staticRuntimeLogSource) ObserveRelease(
	context.Context,
	diagnostics.RuntimeQuery,
) diagnostics.RuntimeObservation {
	return diagnostics.UnavailableSource{}.ObserveRelease(context.Background(), diagnostics.RuntimeQuery{})
}

func (s staticRuntimeLogSource) ReadRuntimeLogs(
	context.Context,
	diagnostics.RuntimeLogQuery,
) (diagnostics.RuntimeLogResult, error) {
	return s.result, s.err
}

func (s staticRuntimeSource) ObserveRelease(
	context.Context,
	diagnostics.RuntimeQuery,
) diagnostics.RuntimeObservation {
	return s.observation
}

func TestReleaseReportDistinguishesAnOlderRunningRelease(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-release-relation")
	older := createReleaseForTarget(t, environment, target.ID, "diagnostics-older")
	requested := createReleaseForTarget(t, environment, target.ID, "diagnostics-requested")

	olderReleaseID := uuid.MustParse(older.Release.ID)
	observedAt := time.Date(2026, time.September, 9, 10, 30, 0, 0, time.UTC)
	db := openTestDatabase(t, environment.databaseURL)
	module := diagnostics.New(
		db,
		projectauth.New(db),
		staticRuntimeSource{observation: diagnostics.RuntimeObservation{
			Workload: diagnostics.WorkloadObservation{
				Metadata: diagnostics.ObservationMetadata{
					Source:     diagnostics.SourceKubernetes,
					ObservedAt: observedAt,
					Status:     diagnostics.ObservationComplete,
				},
				Deployment: &diagnostics.DeploymentEvidence{
					Name:             "orbitops-" + target.ID,
					OwnershipMatches: true,
					ReleaseID:        &olderReleaseID,
				},
			},
			Events: diagnostics.EventObservation{
				Metadata: diagnostics.ObservationMetadata{
					Source:     diagnostics.SourceKubernetes,
					ObservedAt: observedAt,
					Status:     diagnostics.ObservationComplete,
				},
				Items: []diagnostics.EventEvidence{},
			},
		}},
	)

	report, err := module.GetReleaseReport(context.Background(), diagnostics.GetReleaseReportQuery{
		ReleaseID: uuid.MustParse(requested.Release.ID),
		ActorID:   "local-developer",
	})
	if err != nil {
		t.Fatalf("get release diagnostic report: %v", err)
	}

	if report.Release.ID != uuid.MustParse(requested.Release.ID) {
		t.Fatalf("report release = %s, want %s", report.Release.ID, requested.Release.ID)
	}
	if report.Operation.ID != uuid.MustParse(requested.Operation.ID) {
		t.Fatalf("report operation = %s, want %s", report.Operation.ID, requested.Operation.ID)
	}
	if report.RuntimeReleaseRelation != diagnostics.RuntimeReleaseDifferent {
		t.Fatalf(
			"runtime release relation = %q, want %q",
			report.RuntimeReleaseRelation,
			diagnostics.RuntimeReleaseDifferent,
		)
	}
	if !containsDiagnosticSignal(report.Signals, diagnostics.SignalRuntimeReleaseDifferent) {
		t.Fatalf("diagnostic signals = %#v, want runtime release difference", report.Signals)
	}
}

func TestReleaseReportRetainsControlPlaneEvidenceWhenKubernetesIsUnavailable(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-unavailable")
	acceptance := createReleaseForTarget(t, environment, target.ID, "diagnostics-unavailable")
	db := openTestDatabase(t, environment.databaseURL)
	module := diagnostics.New(db, projectauth.New(db), diagnostics.UnavailableSource{})

	report, err := module.GetReleaseReport(context.Background(), diagnostics.GetReleaseReportQuery{
		ReleaseID: uuid.MustParse(acceptance.Release.ID),
		ActorID:   "local-developer",
	})
	if err != nil {
		t.Fatalf("get unavailable runtime report: %v", err)
	}
	if report.Operation.ID != uuid.MustParse(acceptance.Operation.ID) {
		t.Fatalf("operation ID = %s, want %s", report.Operation.ID, acceptance.Operation.ID)
	}
	if report.Workload.Metadata.Status != diagnostics.ObservationUnavailable {
		t.Fatalf("workload status = %q, want unavailable", report.Workload.Metadata.Status)
	}
	if report.RuntimeReleaseRelation != diagnostics.RuntimeReleaseUnknown {
		t.Fatalf("runtime relation = %q, want unknown", report.RuntimeReleaseRelation)
	}
	if !containsDiagnosticSignal(report.Signals, diagnostics.SignalRuntimeObservationUnavailable) {
		t.Fatalf("diagnostic signals = %#v, want runtime unavailable", report.Signals)
	}
}

func TestReleaseReportHidesReleaseFromNonMember(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-non-member")
	acceptance := createReleaseForTarget(t, environment, target.ID, "diagnostics-non-member")
	db := openTestDatabase(t, environment.databaseURL)
	module := diagnostics.New(db, projectauth.New(db), diagnostics.UnavailableSource{})

	_, err := module.GetReleaseReport(context.Background(), diagnostics.GetReleaseReportQuery{
		ReleaseID: uuid.MustParse(acceptance.Release.ID),
		ActorID:   "outside-user",
	})
	if !errors.Is(err, diagnostics.ErrReleaseNotFound) {
		t.Fatalf("non-member error = %v, want ErrReleaseNotFound", err)
	}
}

func TestReleaseReportShowsImmutableTargetDifferences(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-target-difference")
	acceptance := createReleaseForTarget(t, environment, target.ID, "diagnostics-target-difference")
	db := openTestDatabase(t, environment.databaseURL)
	if _, err := db.ExecContext(
		context.Background(),
		`UPDATE deployment_targets SET replicas = 3 WHERE id = $1`,
		target.ID,
	); err != nil {
		t.Fatalf("update current target replicas: %v", err)
	}
	module := diagnostics.New(db, projectauth.New(db), diagnostics.UnavailableSource{})

	report, err := module.GetReleaseReport(context.Background(), diagnostics.GetReleaseReportQuery{
		ReleaseID: uuid.MustParse(acceptance.Release.ID),
		ActorID:   "local-developer",
	})
	if err != nil {
		t.Fatalf("get target difference report: %v", err)
	}
	if !containsTargetDifference(report.TargetDifferences, "replicas", "1", "3") {
		t.Fatalf("target differences = %#v, want replicas 1 -> 3", report.TargetDifferences)
	}
}

func TestReleaseDiagnosticsHTTPExposesTheModuleReport(t *testing.T) {
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		DiagnosticSource: matchingRuntimeSource{},
	})
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-http")
	acceptance := createReleaseForTarget(t, environment, target.ID, "diagnostics-http")

	response := environment.get(t, "/api/v1/releases/"+acceptance.Release.ID+"/diagnostics")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("diagnostics status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var document struct {
		Release struct {
			ID string `json:"id"`
		} `json:"release"`
		RuntimeReleaseRelation string `json:"runtimeReleaseRelation"`
		WorkloadObservation    struct {
			Metadata struct {
				Status string `json:"status"`
			} `json:"metadata"`
		} `json:"workloadObservation"`
		Signals []struct {
			Code string `json:"code"`
		} `json:"signals"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode diagnostics response: %v", err)
	}
	if document.Release.ID != acceptance.Release.ID ||
		document.RuntimeReleaseRelation != string(diagnostics.RuntimeReleaseMatches) ||
		document.WorkloadObservation.Metadata.Status != string(diagnostics.ObservationComplete) {
		t.Fatalf("diagnostics document = %#v", document)
	}
}

func TestRuntimeLogsReturnsASafeBoundedExcerptForDeveloper(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-runtime-logs")
	acceptance := createReleaseForTarget(t, environment, target.ID, "diagnostics-runtime-logs")
	db := openTestDatabase(t, environment.databaseURL)
	source := staticRuntimeLogSource{result: diagnostics.RuntimeLogResult{
		Content:    "token=visible-secret\n" + strings.Repeat("日志", 70_000),
		ObservedAt: time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC),
	}}
	module := diagnostics.New(db, projectauth.New(db), source)

	excerpt, err := module.GetRuntimeLogs(context.Background(), diagnostics.GetRuntimeLogsQuery{
		ReleaseID: uuid.MustParse(acceptance.Release.ID), ActorID: "local-developer",
		PodName: "application-pod", Container: "application", TailLines: 200,
	})
	if err != nil {
		t.Fatalf("get runtime logs: %v", err)
	}
	if strings.Contains(excerpt.Content, "visible-secret") {
		t.Fatalf("runtime logs were not redacted: %.80q", excerpt.Content)
	}
	if len([]byte(excerpt.Content)) > 128*1024 || !excerpt.Truncated {
		t.Fatalf("runtime log bytes = %d, truncated = %t", len([]byte(excerpt.Content)), excerpt.Truncated)
	}
	if excerpt.PodName != "application-pod" || excerpt.Container != "application" || excerpt.TailLines != 200 {
		t.Fatalf("runtime log excerpt = %#v", excerpt)
	}
}

func TestRuntimeLogsRejectsViewer(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-viewer-logs")
	acceptance := createReleaseForTarget(t, environment, target.ID, "diagnostics-viewer-logs")
	db := openTestDatabase(t, environment.databaseURL)
	authorizer := projectauth.New(db)
	if _, err := authorizer.AddMember(context.Background(), projectauth.AddMemberCommand{
		ProjectID: uuid.MustParse(target.ProjectID), MemberActorID: "diagnostics-viewer",
		Role: projectauth.RoleViewer, ActorID: "local-developer",
		IdempotencyKey: "diagnostics-add-viewer",
	}); err != nil {
		t.Fatalf("add diagnostics viewer: %v", err)
	}
	module := diagnostics.New(db, authorizer, staticRuntimeLogSource{})

	_, err := module.GetRuntimeLogs(context.Background(), diagnostics.GetRuntimeLogsQuery{
		ReleaseID: uuid.MustParse(acceptance.Release.ID), ActorID: "diagnostics-viewer",
		PodName: "application-pod", Container: "application", TailLines: 200,
	})
	if !errors.Is(err, projectauth.ErrForbidden) {
		t.Fatalf("viewer runtime log error = %v, want ErrForbidden", err)
	}
}

func TestRuntimeLogsHTTPReturnsTheProtectedExcerpt(t *testing.T) {
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		DiagnosticSource: staticRuntimeLogSource{result: diagnostics.RuntimeLogResult{
			Content:    "application started\n",
			ObservedAt: time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC),
		}},
	})
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-logs-http")
	acceptance := createReleaseForTarget(t, environment, target.ID, "diagnostics-logs-http")

	response := environment.get(
		t,
		"/api/v1/releases/"+acceptance.Release.ID+
			"/runtime-logs?podName=application-pod&container=application&tailLines=7&previous=true",
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("runtime logs status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var document struct {
		ReleaseID string `json:"releaseId"`
		PodName   string `json:"podName"`
		Container string `json:"container"`
		TailLines int    `json:"tailLines"`
		Previous  bool   `json:"previous"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode runtime logs response: %v", err)
	}
	if document.ReleaseID != acceptance.Release.ID || document.PodName != "application-pod" ||
		document.Container != "application" || document.TailLines != 7 || !document.Previous ||
		document.Content != "application started\n" {
		t.Fatalf("runtime logs document = %#v", document)
	}
}

func TestRuntimeLogsHTTPReturnsForbiddenForViewer(t *testing.T) {
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		DiagnosticSource: staticRuntimeLogSource{},
	})
	target := createDeploymentTargetWithSuffix(t, environment, "diagnostics-logs-viewer-http")
	acceptance := createReleaseForTarget(t, environment, target.ID, "diagnostics-logs-viewer-http")
	db := openTestDatabase(t, environment.databaseURL)
	authorizer := projectauth.New(db)
	if _, err := authorizer.AddMember(context.Background(), projectauth.AddMemberCommand{
		ProjectID: uuid.MustParse(target.ProjectID), MemberActorID: "diagnostics-http-viewer",
		Role: projectauth.RoleViewer, ActorID: "local-developer",
		IdempotencyKey: "diagnostics-http-viewer",
	}); err != nil {
		t.Fatalf("add HTTP diagnostics viewer: %v", err)
	}
	viewerServer := environment.serverForActor(t, "diagnostics-http-viewer")
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		viewerServer.URL+"/api/v1/releases/"+acceptance.Release.ID+
			"/runtime-logs?podName=application-pod&container=application",
		nil,
	)
	if err != nil {
		t.Fatalf("build viewer runtime log request: %v", err)
	}
	response, err := viewerServer.Client().Do(request)
	if err != nil {
		t.Fatalf("get viewer runtime logs: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer runtime logs status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	var document errorDocument
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode viewer runtime log error: %v", err)
	}
	if document.Code != "runtime_logs_forbidden" {
		t.Fatalf("viewer error code = %q, want runtime_logs_forbidden", document.Code)
	}
}

func containsDiagnosticSignal(signals []diagnostics.Signal, code diagnostics.SignalCode) bool {
	for _, signal := range signals {
		if signal.Code == code {
			return true
		}
	}
	return false
}

func containsTargetDifference(
	differences []diagnostics.TargetDifference,
	field string,
	releaseValue string,
	currentValue string,
) bool {
	for _, difference := range differences {
		if difference.Field == field &&
			difference.ReleaseValue == releaseValue &&
			difference.CurrentValue == currentValue {
			return true
		}
	}
	return false
}
