package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/google/uuid"
)

const maxFailureSummaryLength = 512

type Config struct {
	WorkerID         string
	LeaseDuration    time.Duration
	OperationTimeout time.Duration
}

type PublishRequest struct {
	OperationID        uuid.UUID
	AttemptID          uuid.UUID
	ReleaseID          uuid.UUID
	ProjectID          uuid.UUID
	ApplicationID      uuid.UUID
	DeploymentTargetID uuid.UUID
	ImageReference     string
	Stage              string
	ClusterRef         string
	Namespace          string
	Replicas           int
	ContainerPort      int
}

type Publisher interface {
	Publish(ctx context.Context, request PublishRequest) error
}

type FailureError struct {
	category string
	summary  string
}

func NewFailure(category string, summary string) *FailureError {
	return &FailureError{category: category, summary: summary}
}

func (e *FailureError) Error() string {
	return e.summary
}

func (e *FailureError) Category() string {
	return e.category
}

func (e *FailureError) Summary() string {
	return e.summary
}

type Runner struct {
	config     Config
	operations *operation.Module
	releases   *delivery.Module
	publisher  Publisher
}

func New(
	config Config,
	operations *operation.Module,
	releases *delivery.Module,
	publisher Publisher,
) (*Runner, error) {
	if config.WorkerID == "" {
		return nil, errors.New("worker ID is required")
	}
	if config.LeaseDuration <= 0 {
		return nil, errors.New("lease duration must be positive")
	}
	if config.OperationTimeout <= 0 {
		return nil, errors.New("operation timeout must be positive")
	}
	if operations == nil {
		return nil, errors.New("operation module is required")
	}
	if releases == nil {
		return nil, errors.New("delivery module is required")
	}
	if publisher == nil {
		return nil, errors.New("publisher is required")
	}

	return &Runner{
		config:     config,
		operations: operations,
		releases:   releases,
		publisher:  publisher,
	}, nil
}

func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	lease, claimed, err := r.operations.ClaimNext(ctx, operation.ClaimRequest{
		WorkerID:      r.config.WorkerID,
		LeaseDuration: r.config.LeaseDuration,
	})
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}

	release, err := r.releases.GetRelease(ctx, lease.ReleaseID)
	if err != nil {
		failure := operation.Failure{
			Category: "release_load_failed",
			Summary:  "accepted release could not be loaded for delivery",
		}
		if completionErr := r.operations.Fail(ctx, lease, failure); completionErr != nil {
			return true, errors.Join(err, completionErr)
		}
		return true, fmt.Errorf("load claimed release: %w", err)
	}

	request := PublishRequest{
		OperationID:        lease.OperationID,
		AttemptID:          lease.AttemptID,
		ReleaseID:          release.ID,
		ProjectID:          release.TargetSnapshot.ProjectID,
		ApplicationID:      release.TargetSnapshot.ApplicationID,
		DeploymentTargetID: release.DeploymentTargetID,
		ImageReference:     release.ImageReference,
		Stage:              release.TargetSnapshot.Stage,
		ClusterRef:         release.TargetSnapshot.ClusterRef,
		Namespace:          release.TargetSnapshot.Namespace,
		Replicas:           release.TargetSnapshot.Replicas,
		ContainerPort:      release.TargetSnapshot.ContainerPort,
	}

	executionContext, cancelExecution := context.WithTimeout(ctx, r.config.OperationTimeout)
	heartbeatContext, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := r.maintainLease(heartbeatContext, lease)
		if heartbeatErr != nil {
			cancelExecution()
		}
		heartbeatDone <- heartbeatErr
	}()

	publishErr := r.publisher.Publish(executionContext, request)
	executionErr := executionContext.Err()
	cancelExecution()
	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}

	if publishErr == nil && executionErr == nil {
		if err := r.operations.Succeed(ctx, lease); err != nil {
			return true, err
		}
		return true, nil
	}

	failure := classifyFailure(publishErr, executionErr)
	if err := r.operations.Fail(ctx, lease, failure); err != nil {
		return true, err
	}
	return true, nil
}

func (r *Runner) maintainLease(ctx context.Context, lease operation.Lease) error {
	interval := r.config.LeaseDuration / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var err error
			lease, err = r.operations.Renew(ctx, lease, r.config.LeaseDuration)
			if err != nil {
				return fmt.Errorf("renew claimed operation: %w", err)
			}
		}
	}
}

func classifyFailure(publishErr error, executionErr error) operation.Failure {
	if errors.Is(executionErr, context.DeadlineExceeded) ||
		errors.Is(publishErr, context.DeadlineExceeded) {
		return operation.Failure{
			Category: "rollout_timeout",
			Summary:  "delivery did not reach a terminal result before the operation timeout",
		}
	}

	var failure *FailureError
	if errors.As(publishErr, &failure) && failure.category != "" && failure.summary != "" {
		return operation.Failure{
			Category: safeText(failure.category, 64),
			Summary:  safeText(failure.summary, maxFailureSummaryLength),
		}
	}

	return operation.Failure{
		Category: "delivery_failed",
		Summary:  "delivery publisher returned an unexpected error",
	}
}

func safeText(value string, maximumLength int) string {
	runes := []rune(value)
	if len(runes) <= maximumLength {
		return value
	}
	return string(runes[:maximumLength])
}
