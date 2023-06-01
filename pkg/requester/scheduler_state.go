package requester

import (
	"context"
	"fmt"

	"github.com/bacalhau-project/bacalhau/pkg/jobstore"
	"github.com/bacalhau-project/bacalhau/pkg/model"
	"github.com/bacalhau-project/bacalhau/pkg/verifier"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/multierr"
)

// TransitionJobState checks the current state of the job and transitions it to the next state if possible, along with triggering
// actions needed to transition, such as updating the job state and notifying compute nodes.
// This method is agnostic to how it was called to allow using the same logic as a response to callback from a compute node, or
// as a result of a periodic check that checks for stale jobs.
func (s *BaseScheduler) TransitionJobState(ctx context.Context, jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitionJobStateLockFree(ctx, jobID)
}

func (s *BaseScheduler) transitionJobStateLockFree(ctx context.Context, jobID string) {
	ctx = log.Ctx(ctx).With().Str("JobID", jobID).Logger().WithContext(ctx)

	jobState, err := s.jobStore.GetJobState(ctx, jobID)
	if err != nil {
		log.Ctx(ctx).Error().Err(err).Msg("[transitionJobState] failed to get job state")
		return
	}

	if jobState.State.IsTerminal() {
		log.Ctx(ctx).Debug().Msgf("[transitionJobState] job %s is already in terminal state %s", jobID, jobState.State)
		return
	}

	job, err := s.jobStore.GetJob(ctx, jobID)
	if err != nil {
		log.Ctx(ctx).Error().Err(err).Msg("[transitionJobState] failed to get job")
		return
	}

	s.checkForFailedExecutions(ctx, job, jobState)
	s.checkForPendingBids(ctx, job, jobState)
	s.checkForPendingResults(ctx, job, jobState)
	s.checkForCompletedExecutions(ctx, job, jobState)
}

// checkForFailedExecutions checks if any execution has failed and if so, check if executions can be retried,
// or transitions the job to a failed state.
func (s *BaseScheduler) checkForFailedExecutions(ctx context.Context, job model.Job, jobState model.JobState) {
	nodesToRetry, err := s.nodeSelector.SelectNodesForRetry(ctx, &job, &jobState)
	canRetry := s.retryStrategy.ShouldRetry(ctx, RetryRequest{JobID: job.ID()})
	if err != nil || (len(nodesToRetry) > 0 && !canRetry) {
		var finalErr error
		var errMsg string

		var lastFailedExecution model.ExecutionState
		for _, execution := range jobState.Executions {
			if execution.State == model.ExecutionStateFailed && lastFailedExecution.UpdateTime.Before(execution.UpdateTime) {
				lastFailedExecution = execution
				finalErr = multierr.Append(
					finalErr,
					fmt.Errorf("node %s failed due to: %s", lastFailedExecution.NodeID, lastFailedExecution.Status),
				)
				errMsg = finalErr.Error()
			}
		}

		s.stopJob(ctx, job.ID(), errMsg, false)
	} else if len(nodesToRetry) > 0 {
		s.notifyAskForBid(ctx, trace.LinkFromContext(ctx), job, nodesToRetry)
	}
}

// checkForPendingBids checks if any bid is still pending a response, if minBids criteria is met, and accept/reject bids accordingly.
func (s *BaseScheduler) checkForPendingBids(ctx context.Context, job model.Job, jobState model.JobState) {
	acceptBids, rejectBids := s.nodeSelector.SelectBids(ctx, &job, &jobState)
	for _, bid := range acceptBids {
		s.updateAndNotifyBidAccepted(ctx, bid)
	}
	for _, bid := range rejectBids {
		s.updateAndNotifyBidRejected(ctx, bid)
	}
}

// checkForPendingResults checks if enough executions proposed a result, verify the results, and accept/reject results accordingly.
func (s *BaseScheduler) checkForPendingResults(ctx context.Context, job model.Job, jobState model.JobState) {
	executionsByState := jobState.GroupExecutionsByState()
	awaitingVerification := len(executionsByState[model.ExecutionStateResultProposed])

	// As long as we have one execution waiting, we can attempt verification.
	// Different verifiers have different thresholds and they will report that
	// they need more completed executions if necessary.
	if awaitingVerification >= 1 {
		succeeded, failed, err := s.verifyResult(ctx, job, executionsByState[model.ExecutionStateResultProposed])
		log.Ctx(ctx).Debug().Err(err).Int("Succeeded", len(succeeded)).Int("Failed", len(failed)).Msg("Attempted to verify results")
		if errors.As(err, new(verifier.ErrInsufficientExecutions)) {
			// OK – we just don't have enough executions for this verifier yet.
			// We will try again when we get some more.
			return
		} else if err != nil {
			s.stopJob(ctx, job.ID(), fmt.Sprintf("failed to verify job %s: %s", job.ID(), err), false)
			return
		}
		if len(failed) > 0 {
			s.transitionJobStateLockFree(ctx, job.ID())
		}
	}
}

// checkForPendingPublishing checks if all verified executions have published, and if so, transition the job to a completed state.
func (s *BaseScheduler) checkForCompletedExecutions(ctx context.Context, job model.Job, jobState model.JobState) {
	shouldUpdate, newState := s.nodeSelector.CanCompleteJob(ctx, &job, &jobState)
	if shouldUpdate {
		err := s.jobStore.UpdateJobState(ctx, jobstore.UpdateJobStateRequest{
			JobID:    job.ID(),
			NewState: newState,
		})
		if err != nil {
			log.Ctx(ctx).Error().Err(err).Msgf("[checkForCompletedExecutions] failed to update job state")
			return
		} else {
			msg := fmt.Sprintf("job %s completed successfully", job.ID())
			if newState == model.JobStateCompletedPartially {
				msg += " partially with some failed executions"
			}
			log.Ctx(ctx).Info().Msg(msg)
		}
	}
}
