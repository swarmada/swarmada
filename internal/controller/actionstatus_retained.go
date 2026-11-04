/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

var _ controlstream.RetainedOutcomeIngestor = (*ActionStatusIngestor)(nil)

// estopEndCauseMessage is the action's message when a retained FAILED says an emergency stop
// ended the action and the control plane records no stop over the robot: the robot's word for
// a stop the control plane did not issue or has not recorded (an edge stop, a local latch).
const estopEndCauseMessage = "paused by estop — the robot's retained outcome says an emergency stop ended the " +
	"action (ACTION_END_CAUSE_ESTOP), and the control plane records no stop over the robot: a stop outcome, " +
	"not a failure, so it is not retried; an operator resume, a cancel, or a requeue is required"

// liveEstopEndCauseMessage is estopEndCauseMessage for a report on the live path: the same stop
// outcome, told by the report the adapter sent as the action ended.
const liveEstopEndCauseMessage = "paused by estop — the robot reported that an emergency stop ended the action " +
	"(ACTION_END_CAUSE_ESTOP), and the control plane records no stop over the robot: a stop outcome, " +
	"not a failure, so it is not retried; an operator resume, a cancel, or a requeue is required"

// IngestRetainedOutcome reads an outcome the adapter retained from before this connection and
// sent with retained = true (RFC-0001 #fleet-adapter-protocol-reconnect-action-state-reconciliation,
// The retained outcome), as rows A13 and A25 of RFC-0001 #safety-action-table decide it, and
// reports whether it was recorded or decided (controlstream.RetainedOutcomeIngestor).
//
// A13: a retained outcome that carries the report token of the action bound to the robot is the
// robot's report from before the event, and A25 decides what it does:
//   - SUCCEEDED ends the work, under a stop or not: the action goes Succeeded from Assigned,
//     InProgress, Paused, Revoking or Preempted, and the reconciler's terminal branch releases
//     the binding and the zone slot.
//   - FAILED under a stop, or carrying ACTION_END_CAUSE_ESTOP, is A1's: an Assigned or InProgress
//     action is paused with its robot, and a Paused, Revoking or Preempted one stays as it is.
//     Both are decided.
//   - FAILED otherwise is the robot's own end report: the action goes Failed.
//
// Acknowledged: those, an action already Succeeded, Failed or Cancelled (a row that ran first
// decided it, or an earlier connection wrote this outcome and its acknowledgement was lost), and
// an action the API server no longer holds. Not acknowledged, so the adapter reports the outcome
// again on its next reconnect: a report that is not terminal; a report token that does not match
// the action's assignment; a Pending action (another row moved the work first, so the report
// would record only the start, which is not done here); an action no longer bound to a robot; a
// stream that may not act on the bound robot; a CANCELLED outcome for a live action (no row reads
// one); and a write that lost to newer writes every time it was tried.
//
// The action is always read from the API server. A retained outcome comes once per reconnect, and
// a cache that has not seen the latest assignment would make a real token look stale.
func (i *ActionStatusIngestor) IngestRetainedOutcome(ctx context.Context, namespace string,
	authorize controlstream.RobotAuthorization, u *fav1.ActionStatusUpdate) (bool, error) {
	if u == nil || u.GetActionId() == "" {
		return false, nil
	}
	if !isTerminalReport(u.GetState()) {
		log.FromContext(ctx).Info("refusing a retained outcome that is not terminal; not acknowledged",
			"action", u.GetActionId(), "namespace", namespace, "reportedState", u.GetState().String())
		return false, nil
	}
	var ack bool
	var action *fleetv1.FleetAction
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error
		ack, action, err = i.retainedOnce(ctx, namespace, authorize, u)
		return err
	})
	if apierrors.IsConflict(err) {
		i.recordReportNotApplied(ctx, action, u,
			"a retained outcome's write lost to newer writes of the action every time; not acknowledged")
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ack, nil
}

// retainedOnce decides u against one read of its action from the API server, writes what it
// decides, and reports whether the outcome is acknowledged.
func (i *ActionStatusIngestor) retainedOnce(ctx context.Context, namespace string,
	authorize controlstream.RobotAuthorization, u *fav1.ActionStatusUpdate) (bool, *fleetv1.FleetAction, error) {
	logger := log.FromContext(ctx).WithValues("action", u.GetActionId(), "namespace", namespace,
		"reportedState", u.GetState().String(), "retained", true)
	action := &fleetv1.FleetAction{}
	if i.APIReader == nil {
		// Only the API server's copy decides a retained outcome; a cached one could make a real
		// token look stale, or an outcome already decided look open. Nothing is acknowledged.
		return false, action, errors.New("retained outcome not read: no API reader; not acknowledged")
	}
	if err := i.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: u.GetActionId()}, action); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("retained outcome for an action the API server no longer holds: nothing to write; acknowledged")
			return true, action, nil
		}
		return false, action, err
	}

	// The report token ties the outcome to one run of the action. A report without one, or with
	// another run's, is not this action's outcome; it is refused and left with the adapter.
	if u.FencingToken == nil {
		i.refuseTokenless(ctx, action, u)
		return false, action, nil
	}
	if gen, ok := command.GenerationFromToken(u.GetFencingToken()); !ok || gen != action.Status.AssignmentGeneration {
		// Another run's report. Its refusal is raised on the bound robot, and only once the stream
		// is shown to serve that robot: an unauthorized stream, or an action bound to no robot,
		// gets a log line only, so no stream can put a Warning on an object it names.
		var on client.Object
		if action.Status.AssignedRobot != "" {
			if _, ok := i.streamServesAssignedRobot(ctx, authorize, action); ok {
				on = i.boundRobotRef(ctx, action)
			}
		}
		i.recordReportRefused(ctx, action, u, fmt.Sprintf(
			"retained outcome for action %s: fencing token %d does not match assignment generation %d; not acknowledged",
			action.Name, u.GetFencingToken(), action.Status.AssignmentGeneration), on)
		return false, action, nil
	}

	switch action.Status.Phase {
	case fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseFailed, fleetv1.ActionPhaseCancelled:
		// Already decided. Nothing is written, and the stream is not asked about the robot: a
		// terminal action's binding is released, and the acknowledgement only tells the sender to
		// stop retaining an outcome it sent, for a run the token names.
		logger.Info("retained outcome for an action already ended: nothing to write; acknowledged",
			"phase", action.Status.Phase)
		return true, action, nil
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress, fleetv1.ActionPhasePaused,
		fleetv1.ActionPhaseRevoking, fleetv1.ActionPhasePreempted:
	default:
		i.recordReportRefused(ctx, action, u, fmt.Sprintf(
			"retained outcome for a %s action is not read; not acknowledged", action.Status.Phase), nil)
		return false, action, nil
	}
	if action.Status.AssignedRobot == "" {
		i.recordReportRefused(ctx, action, u,
			"retained outcome: the action is no longer bound to a robot; not acknowledged", nil)
		return false, action, nil
	}
	if why, ok := i.streamServesAssignedRobot(ctx, authorize, action); !ok {
		i.recordReportRefused(ctx, action, u, why, nil)
		return false, action, nil
	}

	base := action.DeepCopy()
	var pausedUnder []string
	switch u.GetState() {
	case fav1.ActionState_ACTION_STATE_SUCCEEDED:
		now := metav1.Now()
		action.Status.Phase = fleetv1.ActionPhaseSucceeded
		action.Status.CompletedAt = &now
		action.Status.CompletionTime = &now
		action.Status.ProgressPct = 100
		if action.Status.StartedAt == nil {
			action.Status.StartedAt = &now
		}
		if u.GetMessage() != "" {
			action.Status.Message = u.GetMessage()
		}

	case fav1.ActionState_ACTION_STATE_FAILED:
		stops, err := i.stopsOver(ctx, action)
		if err != nil {
			return false, action, err
		}
		byEstop := u.GetEndCause() == fav1.ActionEndCause_ACTION_END_CAUSE_ESTOP
		if len(stops) > 0 || byEstop {
			if action.Status.Phase != fleetv1.ActionPhaseAssigned && action.Status.Phase != fleetv1.ActionPhaseInProgress {
				logger.Info("retained failure under a stop is A1's and moves nothing; acknowledged",
					"phase", action.Status.Phase, "stops", stops, "endCause", u.GetEndCause().String())
				return true, action, nil
			}
			reason := u.GetMessage()
			if reason == "" {
				reason = "robot reported action failure"
			}
			action.Status.Phase = fleetv1.ActionPhasePaused
			if len(stops) > 0 {
				action.Status.Message = failedUnderStopMessage(reason, stops)
				pausedUnder = stops
			} else {
				action.Status.Message = estopEndCauseMessage
				pausedUnder = []string{"ACTION_END_CAUSE_ESTOP"}
			}
			break
		}
		now := metav1.Now()
		action.Status.Phase = fleetv1.ActionPhaseFailed
		action.Status.FailedAt = &now
		action.Status.CompletionTime = &now
		reason := u.GetMessage()
		if reason == "" {
			reason = "robot reported action failure"
		}
		action.Status.FailureReason = reason
		if u.GetMessage() != "" {
			action.Status.Message = u.GetMessage()
		}

	default: // CANCELLED for a live action
		i.recordReportRefused(ctx, action, u,
			"a retained CANCELLED outcome for a live action is not read; not acknowledged", nil)
		return false, action, nil
	}

	// Written with the resourceVersion it was decided on, as every report is: a conflict decides
	// it again on a fresh read.
	if err := i.Status().Patch(ctx, action, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, base, err
	}
	logger.Info("retained outcome recorded; acknowledged", "from", base.Status.Phase, "to", action.Status.Phase)
	if len(pausedUnder) > 0 {
		warnActionAndTask(i.Recorder, action, EventActionFailedUnderEstop, action.Status.Message)
		i.recordPausedByEstop(ctx, action, base.Status.Phase)
	}
	return true, action, nil
}
