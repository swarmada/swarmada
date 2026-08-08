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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ActionStatusIngestor advances the FleetAction lifecycle from a robot's reported
// action state, delivered over the ControlStream as a ActionStatusUpdate (§9.2.3).
//
// It writes the action-object side of every reported transition: RUNNING →
// InProgress (startedAt), SUCCEEDED → Succeeded (completedAt), and FAILED →
// Failed (failedAt, failureReason). It deliberately does NOT free the robot,
// drop the assignment lease, or release reservations: freeing a physical robot
// for reassignment is governed by the single-executor lease lifecycle in the
// reconciler and is applied there. Releasing the terminal action's TDE zone
// reservation is handled by the reconciler's idempotent terminal branch.
type ActionStatusIngestor struct {
	client.Client
}

// IngestActionStatus applies one ActionStatusUpdate to the named action. It is a no-op
// (returning nil) when the action is absent, the update is fenced out, the action is
// not in an advanceable phase, or the reported state is one this increment does
// not yet act on. Errors are returned only for unexpected API failures.
func (i *ActionStatusIngestor) IngestActionStatus(ctx context.Context, namespace string, u *fav1.ActionStatusUpdate) error {
	if u == nil || u.GetActionId() == "" {
		return nil
	}

	action := &fleetv1.FleetAction{}
	if err := i.Get(ctx, client.ObjectKey{Namespace: namespace, Name: u.GetActionId()}, action); err != nil {
		// A status update for an unknown action is stale, not an error.
		return client.IgnoreNotFound(err)
	}

	// Single-executor fencing: a robot still reporting under a superseded
	// assignment must not move the current one. An unset token is treated as
	// "current" for backward compatibility with adapters that omit it.
	// A token that cannot be represented as a generation cannot match one either — treated as a
	// mismatch (stale) rather than wrapped into a negative that might compare equal by accident.
	tokenGen, representable := command.GenerationFromToken(u.GetFencingToken())
	if u.FencingToken != nil && (!representable || tokenGen != action.Status.AssignmentGeneration) {
		return nil
	}

	// Only advance a action the control plane still considers actively owned by a
	// robot. Any hold or terminal phase (Revoking/Cancelled/Succeeded/Failed) is
	// authoritative and must not be overwritten by a robot report.
	switch action.Status.Phase {
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress:
	default:
		return nil
	}

	base := action.DeepCopy()

	switch u.GetState() {
	case fav1.ActionState_ACTION_STATE_RUNNING:
		pct := clampPct(u.GetProgressPct())
		// Idempotent: already executing with startedAt, and neither the message nor
		// the reported progress changed → no-op (RA-1: no status write per tick).
		if action.Status.Phase == fleetv1.ActionPhaseInProgress &&
			action.Status.StartedAt != nil &&
			action.Status.ProgressPct == pct &&
			(u.GetMessage() == "" || u.GetMessage() == action.Status.Message) {
			return nil
		}
		action.Status.Phase = fleetv1.ActionPhaseInProgress
		if action.Status.StartedAt == nil {
			startedTS := metav1.Now()
			action.Status.StartedAt = &startedTS
		}
		action.Status.ProgressPct = pct
		if u.GetMessage() != "" {
			action.Status.Message = u.GetMessage()
		}

	case fav1.ActionState_ACTION_STATE_SUCCEEDED:
		now := metav1.Now()
		action.Status.Phase = fleetv1.ActionPhaseSucceeded
		action.Status.CompletedAt = &now
		action.Status.CompletionTime = &now
		action.Status.ProgressPct = 100
		// A action that completed within a single report may never have stamped
		// startedAt; back-fill it so the lifecycle is well-formed.
		if action.Status.StartedAt == nil {
			action.Status.StartedAt = &now
		}
		if u.GetMessage() != "" {
			action.Status.Message = u.GetMessage()
		}

	case fav1.ActionState_ACTION_STATE_FAILED:
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

	default:
		// PAUSED / CANCELLED / UNSPECIFIED are not driven from a robot report here:
		// pause is a capability concern and cancellation is control-plane-driven
		// with confirmed-stop semantics.
		return nil
	}

	return i.Status().Patch(ctx, action, client.MergeFrom(base))
}

// clampPct bounds a reported progress percentage into [0,100]; a malformed
// out-of-range report is clamped rather than rejected.
func clampPct(p int32) int32 {
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	default:
		return p
	}
}
