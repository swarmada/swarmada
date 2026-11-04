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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"
)

// conditionRenewalHeld is the condition, and reasonStopClearedBeforePause its reason, of an
// Assigned or InProgress action whose robot's stop was cleared before row A1 paused it. Row A1
// reads a robot under a stop, which a Resuming robot is not, so the work was never paused, and
// the lease decision would renew it on the wire. reasonStopClearedWhilePaused is its reason on a
// Paused action whose robot's stop was cleared while the action was bound to it. The clear and the
// release write the same condition on the work the robot records (safety.ConditionRenewalHeld).
const (
	conditionRenewalHeld         = safety.ConditionRenewalHeld
	reasonStopClearedBeforePause = safety.ReasonStopClearedBeforePause
	reasonStopClearedWhilePaused = safety.ReasonStopClearedWhilePaused
)

// leaseStampHeld reports whether a clear holds the work's lease where it stands: the work carries
// RenewalHeld. A stop the robot confirms after the clear does not settle the work, so row A1 then
// keeps the lease it pauses the work with rather than stamping a new one, and the Paused branch
// does not advance it.
func leaseStampHeld(action *fleetv1.FleetAction) bool {
	return apimeta.FindStatusCondition(action.Status.Conditions, conditionRenewalHeld) != nil
}

// pausedLeaseHeldAfterClear reports whether the lease of a Paused action bound to robot is held
// after a clear (RFC-0001 #safety-action-table, The lease): the control plane advances no
// leaseExpiresAt for the work the robot records until the robot's own report or an operator's
// decision settles it, whatever the work's phase. The hold holds while the robot is Resuming, and
// after by the condition it set, a later confirmed stop included. Every row out of Paused (A2, A4,
// A25) releases the binding, and the next assignment removes the condition.
func pausedLeaseHeldAfterClear(action *fleetv1.FleetAction, robot *fleetv1.Robot) bool {
	return leaseStampHeld(action) || robot.Status.EstopState == fleetv1.RobotEstopResuming
}

// renewalHeldAfterClear reports whether the work's lease is held after a clear (RFC-0001
// #safety-action-table, The lease): after the clear nothing about the work the robot records
// resumes automatically, lease renewals included, until the robot's own report or an operator's
// decision settles it. The operator release (S6) releases the robot, not its work. So the hold
// holds while the robot is Resuming, and after S6 by the condition it set, which outlives the
// release: the robot is Normal then, and nothing else on it says a stop interrupted the work.
//
// It ends when the action leaves Assigned, InProgress and Revoking: a terminal report (row A25),
// a report of the work ended by the stop (row A1), an operator's cancel (row A3), or a declarative
// hold (row A1). A RUNNING report does not end it. The condition is removed at the next assignment.
func renewalHeldAfterClear(action *fleetv1.FleetAction, robot *fleetv1.Robot, found bool) bool {
	switch action.Status.Phase {
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress, fleetv1.ActionPhaseRevoking:
	default:
		return false
	}
	if apimeta.FindStatusCondition(action.Status.Conditions, conditionRenewalHeld) != nil {
		return true
	}
	return found && robot.Status.EstopState == fleetv1.RobotEstopResuming
}

// holdRenewalAfterClear holds the work's lease: no leaseExpiresAt is stamped and no
// RenewActionLease is sent, so the robot stops on its own lease. The condition RenewalHeld is set,
// with one Warning Event, on the transition only (RA-1), and the action is looked at again by the
// lease horizon.
func (r *FleetActionReconciler) holdRenewalAfterClear(ctx context.Context, action, original *fleetv1.FleetAction,
	robot *fleetv1.Robot, now time.Time, leaseRenew, leaseSkew time.Duration) (ctrl.Result, error) {
	requeue := ctrl.Result{RequeueAfter: min(leaseRenew,
		untilLeaseHorizon(action.Status.LeaseExpiresAt, now, leaseRenew, leaseSkew))}
	if leaseStampHeld(action) {
		return requeue, nil
	}
	msg := fmt.Sprintf("the stop on robot %s was cleared before this action was paused; its lease is not renewed, "+
		"and the robot stops on its own lease, until the robot reports the work ended or an operator cancels, "+
		"pauses or requeues it", robot.Name)
	if err := r.setRenewalHeld(ctx, action, original, reasonStopClearedBeforePause, msg); err != nil {
		return ctrl.Result{}, fmt.Errorf("holding the lease of work a cleared stop interrupted: %w", err)
	}
	log.FromContext(ctx).Info("lease renewal held: the robot's stop was cleared before the work was paused",
		"robot", robot.Name, "estopState", robot.Status.EstopState)
	return requeue, nil
}

// holdPausedLeaseAfterClear keeps a Paused action's lease where the clear left it: no
// leaseExpiresAt is advanced, under a later confirmed stop too, until the work is settled
// (pausedLeaseHeldAfterClear). The condition RenewalHeld is set, with one Warning Event, on the
// transition only (RA-1), unless the work already carries it from before it was paused.
func (r *FleetActionReconciler) holdPausedLeaseAfterClear(ctx context.Context, action, original *fleetv1.FleetAction,
	robot *fleetv1.Robot, now time.Time, leaseRenew, leaseSkew time.Duration) (ctrl.Result, error) {
	requeue := ctrl.Result{RequeueAfter: min(leaseRenew,
		untilLeaseHorizon(action.Status.LeaseExpiresAt, now, leaseRenew, leaseSkew))}
	if leaseStampHeld(action) {
		return requeue, nil
	}
	msg := fmt.Sprintf("the stop on robot %s was cleared while this action was Paused; its lease is not advanced, "+
		"under a later stop either, until the robot reports the work ended or an operator cancels or requeues it",
		robot.Name)
	if err := r.setRenewalHeld(ctx, action, original, reasonStopClearedWhilePaused, msg); err != nil {
		return ctrl.Result{}, fmt.Errorf("holding the lease of Paused work after a clear: %w", err)
	}
	log.FromContext(ctx).Info("lease held: the robot's stop was cleared while the work was Paused",
		"robot", robot.Name, "estopState", robot.Status.EstopState)
	return requeue, nil
}

// setRenewalHeld writes the condition RenewalHeld with reason and msg, and raises its Warning Event.
func (r *FleetActionReconciler) setRenewalHeld(ctx context.Context, action, original *fleetv1.FleetAction,
	reason, msg string) error {
	apimeta.SetStatusCondition(&action.Status.Conditions, metav1.Condition{
		Type:    conditionRenewalHeld,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: msg,
	})
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return err
	}
	if r.Recorder != nil {
		r.Recorder.Event(action, corev1.EventTypeWarning, conditionRenewalHeld, msg)
	}
	return nil
}
