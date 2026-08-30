package runtimeview

import (
	"context"
	"time"

	"github.com/google/uuid"
)

const (
	SourceKubernetes     = "kubernetes"
	FreshnessFresh       = "fresh"
	FreshnessUnavailable = "unavailable"
)

type Query struct {
	ClusterRef string
	Namespace  string
	TargetID   uuid.UUID
}

type Snapshot struct {
	Source            string
	ObservedAt        time.Time
	Freshness         string
	DeploymentName    string
	DeploymentExists  bool
	ReleaseID         *uuid.UUID
	DesiredReplicas   int32
	UpdatedReplicas   int32
	ReadyReplicas     int32
	AvailableReplicas int32
	Conditions        []Condition
	Pods              []PodSummary
	ErrorCategory     *string
}

type Condition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

type PodSummary struct {
	Name   string
	Phase  string
	Ready  bool
	Reason string
}

type Observer interface {
	Observe(ctx context.Context, query Query) Snapshot
}

type UnavailableObserver struct{}

func (UnavailableObserver) Observe(_ context.Context, _ Query) Snapshot {
	category := "kubernetes_not_configured"
	return Snapshot{
		Source:        SourceKubernetes,
		ObservedAt:    time.Now().UTC(),
		Freshness:     FreshnessUnavailable,
		Conditions:    []Condition{},
		Pods:          []PodSummary{},
		ErrorCategory: &category,
	}
}
