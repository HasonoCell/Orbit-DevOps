package diagnostics

import (
	"testing"

	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/google/uuid"
)

func TestRelateRuntimeRelease(t *testing.T) {
	t.Parallel()

	requestedID := uuid.New()
	otherID := uuid.New()
	tests := []struct {
		name        string
		observation WorkloadObservation
		want        RuntimeReleaseRelation
	}{
		{
			name: "unavailable",
			observation: WorkloadObservation{Metadata: ObservationMetadata{
				Status: ObservationUnavailable,
			}},
			want: RuntimeReleaseUnknown,
		},
		{
			name: "deployment absent",
			observation: WorkloadObservation{Metadata: ObservationMetadata{
				Status: ObservationComplete,
			}},
			want: RuntimeReleaseAbsent,
		},
		{
			name: "release label absent",
			observation: WorkloadObservation{
				Metadata:   ObservationMetadata{Status: ObservationComplete},
				Deployment: &DeploymentEvidence{Name: "application", OwnershipMatches: true},
			},
			want: RuntimeReleaseUnknown,
		},
		{
			name: "matching release",
			observation: WorkloadObservation{
				Metadata:   ObservationMetadata{Status: ObservationComplete},
				Deployment: &DeploymentEvidence{Name: "application", OwnershipMatches: true, ReleaseID: &requestedID},
			},
			want: RuntimeReleaseMatches,
		},
		{
			name: "different release",
			observation: WorkloadObservation{
				Metadata:   ObservationMetadata{Status: ObservationComplete},
				Deployment: &DeploymentEvidence{Name: "application", OwnershipMatches: true, ReleaseID: &otherID},
			},
			want: RuntimeReleaseDifferent,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := relateRuntimeRelease(requestedID, test.observation); got != test.want {
				t.Fatalf("relation = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDeriveSignalsUsesDeterministicSeverityAndCodeOrder(t *testing.T) {
	t.Parallel()

	report := Report{
		Operation: operation.Record{ID: uuid.New(), Status: operation.StatusFailed},
		Workload: WorkloadObservation{Metadata: ObservationMetadata{
			Status: ObservationUnavailable,
		}},
		RuntimeReleaseRelation: RuntimeReleaseUnknown,
	}

	signals := deriveSignals(report)
	want := []SignalCode{SignalOperationFailed, SignalRuntimeObservationUnavailable}
	if len(signals) != len(want) {
		t.Fatalf("signal count = %d, want %d: %#v", len(signals), len(want), signals)
	}
	for index, code := range want {
		if signals[index].Code != code {
			t.Fatalf("signal[%d] = %q, want %q", index, signals[index].Code, code)
		}
	}
}

func TestDeriveSignalsExplainsIncompleteKubernetesEvidence(t *testing.T) {
	t.Parallel()

	report := Report{
		Operation: operation.Record{ID: uuid.New(), Status: operation.StatusRunning},
		Workload: WorkloadObservation{
			Metadata: ObservationMetadata{
				Status: ObservationPartial, ErrorCategories: []string{"ownership_conflict"},
			},
			Deployment: &DeploymentEvidence{
				UID: "deployment-uid", OwnershipMatches: true,
				Generation: 2, ObservedGeneration: 1, DesiredReplicas: 1,
			},
			Pods: []PodEvidence{{
				UID: "pod-uid", Name: "application-pod",
				Containers: []ContainerEvidence{{
					Name: "application", State: "waiting", Reason: "ImagePullBackOff", RestartCount: 2,
				}},
			}},
		},
		Events: EventObservation{
			Metadata: ObservationMetadata{Status: ObservationComplete},
			Items:    []EventEvidence{{UID: "event-uid", Type: "Warning", Reason: "Failed"}},
		},
	}

	signals := deriveSignals(report)
	for _, code := range []SignalCode{
		SignalResourceOwnershipConflict,
		SignalRolloutIncomplete,
		SignalPodWaiting,
		SignalPodRestarting,
		SignalWarningEventObserved,
	} {
		if !hasSignal(signals, code) {
			t.Errorf("signals = %#v, want %q", signals, code)
		}
	}
}

func hasSignal(signals []Signal, code SignalCode) bool {
	for _, signal := range signals {
		if signal.Code == code {
			return true
		}
	}
	return false
}
