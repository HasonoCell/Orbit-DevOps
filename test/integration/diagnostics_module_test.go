package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
)

// staticRuntimeSource 只在诊断模块 seam 返回已知的 Kubernetes 点时证据。
type staticRuntimeSource struct {
	observation diagnostics.RuntimeObservation
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
					Name:      "orbitops-" + target.ID,
					ReleaseID: &olderReleaseID,
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
