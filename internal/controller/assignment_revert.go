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
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// THE ASSIGNMENT IS NOT TAKEN UP (RFC-0001 #safety-action-table, row A24).
//
// An assignment just committed (the action Assigned, the robot recording it, a TDE slot held) that
// the robot never took up returns to Pending:
//   - the command dispatcher's check at the send refused it, so no AssignAction went on the wire:
//     the robot's register is not yet processed on its adapter's ControlStream
//     (command.ErrNotRegistered), or that stream did not negotiate contract 1.4.0
//     (command.ErrBelowLatchContract). This is A24's second trigger;
//   - the adapter answered the one AssignAction sent at this generation with accepted = false:
//     A24's third trigger.
//
// A stop holding the robot (Resuming included) holds the action instead, on every attempt: row A1
// pauses it on the robot (holdRefusalUnderStop). For the second trigger that is narrower than A24,
// which reverts under a stop too; the reference control plane holds there.
//
// Every other push failure (no stream, a send failure, a timeout, a lost leadership) wraps
// command.ErrUnreachable like a send that may have gone out, so it cannot be told apart from one:
// the assignment stands, and the lease machinery governs (A24: "a missing answer is not a
// rejection"). command.ErrHeldByStop also stands: the stop's own rows decide it (A1).

// notSentReason says why the command dispatcher sent no AssignAction, when the refusal is one A24
// reverts on, or returns "".
func notSentReason(err error) string {
	switch {
	case errors.Is(err, command.ErrNotRegistered):
		return "the robot's register has not been processed on its adapter's ControlStream"
	case errors.Is(err, command.ErrBelowLatchContract):
		return "the adapter's ControlStream did not negotiate contract 1.4.0"
	}
	return ""
}

// revertAssignment returns an assignment the robot never took up to Pending (A24), in the order the
// action table's rules give: the Robot first, then the action, then the zone reservation.
//
//   - The Robot: on each attempt one live read of the robot, which the stop guard judges
//     (holdRefusalUnderStop); a stop holding it holds the action, and nothing is released.
//     Otherwise the write goes to that same copy, under its resourceVersion, and only while it
//     records this action and the live action
//     is still Assigned to it at generation gen. Its record goes back to victim, phase InProgress,
//     when this assignment displaced a victim on this robot that is still Preempted and bound to it
//     (the robot is still running the victim, since the replacement never reached it); otherwise it
//     is cleared, phase Idle. Only assignedAction and phase are written. A conflict re-reads and
//     guards again: a stop recorded on the robot is a likely cause of one.
//   - The action: Pending, assignedRobot and leaseExpiresAt cleared, pendingSince written again (v0.4:
//     it is written on each row whose target phase is Pending), assignmentGeneration kept (the next
//     commit mints a higher one), retryCount unchanged; under its resourceVersion, retried on a
//     conflict while it is still Assigned to this robot at gen.
//   - The zone reservation is released (idempotent).
//
// Victims marked Preempted by this pass, the robot's and any TDE eviction's, stay Preempted: no row
// takes them back, and they leave through row A12.
//
// If a write still fails after its retries, the revert is incomplete, and nothing retries it: the
// pass returns the error, and the next pass sees an Assigned action, not the refusal. If the Robot
// write failed, the robot still records the action, and the lease machinery governs it as any
// assignment whose delivery is unknown (renewals, then the lease horizon). If the action write
// failed after the Robot write, the robot is free and the action is held until its lease horizon,
// then released. Both are safe, since no AssignAction carried the work; they are slower than the
// revert.
//
// reverted is true when this call returned the action to Pending: not when a stop held it, a write
// failed, or another pass had already moved it.
func (r *FleetActionReconciler) revertAssignment(ctx context.Context, action *fleetv1.FleetAction, robotName string,
	gen int64, victim string, message string) (res ctrl.Result, reverted bool, err error) {
	key := client.ObjectKeyFromObject(action)
	held := false
	var holdRes ctrl.Result
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		held, holdRes = false, ctrl.Result{} // each attempt decides afresh
		// One live read per attempt: the stop guard judges this copy, and the write goes under its
		// resourceVersion, so a stop recorded on the robot after the read conflicts the write.
		robot := &fleetv1.Robot{}
		if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: action.Namespace, Name: robotName}, robot); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // no robot record to release
			}
			held = true // a question the guard cannot answer releases nothing
			return fmt.Errorf("reading robot %s for its stop before releasing the assignment: %w", robotName, err)
		}
		res, isHeld, err := r.holdRefusalUnderStop(ctx, action, robot)
		if isHeld || err != nil {
			held, holdRes = isHeld, res
			if apierrors.IsConflict(err) {
				// The pause's own write lost: not a conflict on this loop's Robot write, so it is
				// not retried here; the pass returns it and the next pass decides again.
				err = fmt.Errorf("pausing the action under a stop: %v", err)
			}
			return err
		}
		if robot.Status.AssignedAction != action.Name {
			return nil // the robot no longer records it: nothing to release
		}
		if still, err := r.stillAssigned(ctx, key, robotName, gen); err != nil || !still {
			return err
		}
		base := robot.DeepCopy()
		robot.Status.AssignedAction = ""
		// Only a robot in a work phase is moved: a phase another writer set since the commit (Error,
		// Offline) is left as it is, and no victim is restored onto it.
		working := robot.Status.Phase == fleetv1.RobotPhaseInProgress || robot.Status.Phase == fleetv1.RobotPhaseAssigned
		if working {
			robot.Status.Phase = fleetv1.RobotPhaseIdle
		}
		if victim != "" && working {
			restore, err := r.victimStillOn(ctx, action.Namespace, victim, robotName)
			if err != nil {
				return err
			}
			if restore {
				robot.Status.AssignedAction = victim
				robot.Status.Phase = fleetv1.RobotPhaseInProgress
			}
		}
		return r.Status().Patch(ctx, robot, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
	if held || err != nil {
		if err != nil {
			err = fmt.Errorf("releasing the robot of an assignment not taken up: %w", err)
		}
		return holdRes, false, err
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &fleetv1.FleetAction{}
		if err := r.liveReader().Get(ctx, key, live); err != nil {
			return client.IgnoreNotFound(err)
		}
		if live.Status.Phase != fleetv1.ActionPhaseAssigned || live.Status.AssignedRobot != robotName ||
			live.Status.AssignmentGeneration != gen {
			return nil // another pass has moved it
		}
		base := live.DeepCopy()
		live.Status.Phase = fleetv1.ActionPhasePending
		live.Status.AssignedRobot = ""
		live.Status.LeaseExpiresAt = nil
		n := metav1.Now()
		live.Status.PendingSince = &n
		live.Status.Message = message
		if err := r.patchActionStatus(ctx, live, base); err != nil {
			return err
		}
		*action = *live
		reverted = true
		return nil
	}); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("returning an assignment not taken up to Pending: %w", err)
	}
	r.releaseReservation(ctx, action.Namespace, action.Spec.Zone, action.Name)
	minRetry, _ := r.tdeRetryBounds(ctx, action.Namespace)
	return ctrl.Result{RequeueAfter: minRetry}, reverted, nil
}

// stillAssigned reports whether the live action is still Assigned to robotName at generation gen.
func (r *FleetActionReconciler) stillAssigned(ctx context.Context, key types.NamespacedName, robotName string, gen int64) (bool, error) {
	live := &fleetv1.FleetAction{}
	if err := r.liveReader().Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return live.Status.Phase == fleetv1.ActionPhaseAssigned && live.Status.AssignedRobot == robotName &&
		live.Status.AssignmentGeneration == gen, nil
}

// victimStillOn reports whether the live victim action is still Preempted and bound to robotName,
// so the robot is still running it. A victim released since (row A12) is not restored.
func (r *FleetActionReconciler) victimStillOn(ctx context.Context, namespace, victim, robotName string) (bool, error) {
	v := &fleetv1.FleetAction{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: victim}, v); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return v.Status.Phase == fleetv1.ActionPhasePreempted && v.Status.AssignedRobot == robotName, nil
}

// notSentEvents emits the AssignmentNotSent Event once per action, robot and reason, not on every
// revert while the cause lasts; the key is forgotten once an AssignAction is sent for the action.
type notSentEvents struct {
	mu   sync.Mutex
	seen map[types.NamespacedName]string
}

func (n *notSentEvents) first(key types.NamespacedName, robot, reason string) bool {
	reason = robot + "\x00" + reason
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = map[types.NamespacedName]string{}
	}
	if n.seen[key] == reason {
		return false
	}
	n.seen[key] = reason
	return true
}

func (n *notSentEvents) forget(key types.NamespacedName) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.seen, key)
}

// EventAssignmentNotSent is the Event reason for an assignment the control plane committed and its
// own check at the send refused to send, so the action returned to Pending (A24).
const EventAssignmentNotSent = "AssignmentNotSent"

// recordNotSent logs the not-sent revert and emits its Event once per action and reason.
func (r *FleetActionReconciler) recordNotSent(ctx context.Context, action *fleetv1.FleetAction, robotName, reason string) {
	log.FromContext(ctx).Info("assign_action not sent; the assignment returns to Pending (A24)",
		"task", action.Name, "robot", robotName, "reason", reason)
	if r.Recorder != nil && r.notSent.first(client.ObjectKeyFromObject(action), robotName, reason) {
		r.Recorder.Event(action, corev1.EventTypeNormal, EventAssignmentNotSent,
			fmt.Sprintf("the assignment to robot %s was not sent (%s); the action returns to Pending and is "+
				"retried", robotName, reason))
	}
}
